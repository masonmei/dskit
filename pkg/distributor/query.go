package distributor

import (
	"context"
	"io"
	"sort"

	"github.com/gogo/status"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/weaveworks/common/instrument"
	"github.com/weaveworks/common/user"
	"google.golang.org/grpc/codes"

	"github.com/cortexproject/cortex/pkg/ingester/client"
	ingester_client "github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/extract"
)

// Query multiple ingesters and returns a Matrix of samples.
func (d *Distributor) Query(ctx context.Context, from, to model.Time, matchers ...*labels.Matcher) (model.Matrix, error) {
	var matrix model.Matrix
	err := instrument.CollectedRequest(ctx, "Distributor.Query", queryDuration, instrument.ErrorCode, func(ctx context.Context) error {
		replicationSet, req, err := d.queryPrep(ctx, from, to, matchers...)
		if err != nil {
			return promql.ErrStorage{Err: err}
		}

		matrix, err = d.queryIngesters(ctx, replicationSet, req)
		if err != nil {
			return promql.ErrStorage{Err: err}
		}
		return nil
	})
	return matrix, err
}

// QueryStream multiple ingesters via the streaming interface and returns big ol' set of chunks.
func (d *Distributor) QueryStream(ctx context.Context, from, to model.Time, matchers ...*labels.Matcher) (*ingester_client.QueryStreamResponse, error) {
	var result *ingester_client.QueryStreamResponse
	err := instrument.CollectedRequest(ctx, "Distributor.QueryStream", queryDuration, instrument.ErrorCode, func(ctx context.Context) error {
		replicationSet, req, err := d.queryPrep(ctx, from, to, matchers...)
		if err != nil {
			return promql.ErrStorage{Err: err}
		}

		result, err = d.queryIngesterStream(ctx, replicationSet, req)
		if err != nil {
			return promql.ErrStorage{Err: err}
		}
		return nil
	})
	return result, err
}

func (d *Distributor) queryPrep(ctx context.Context, from, to model.Time, matchers ...*labels.Matcher) (ring.ReplicationSet, *client.QueryRequest, error) {
	var replicationSet ring.ReplicationSet
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return replicationSet, nil, err
	}

	req, err := ingester_client.ToQueryRequest(from, to, matchers)
	if err != nil {
		return replicationSet, nil, err
	}

	// Get ingesters by metricName if one exists, otherwise get all ingesters
	metricNameMatcher, _, ok := extract.MetricNameMatcherFromMatchers(matchers)
	if !d.cfg.ShardByAllLabels && ok && metricNameMatcher.Type == labels.MatchEqual {
		replicationSet, err = d.ingestersRing.Get(shardByMetricName(userID, metricNameMatcher.Value), ring.Read, nil)
	} else {
		replicationSet, err = d.ingestersRing.GetAll()
	}
	return replicationSet, req, err
}

// queryIngesters queries the ingesters via the older, sample-based API.
func (d *Distributor) queryIngesters(ctx context.Context, replicationSet ring.ReplicationSet, req *client.QueryRequest) (model.Matrix, error) {
	// Fetch samples from multiple ingesters in parallel, using the replicationSet
	// to deal with consistency.
	results, err := replicationSet.Do(ctx, d.cfg.ExtraQueryDelay, func(ing *ring.IngesterDesc) (interface{}, error) {
		client, err := d.ingesterPool.GetClientFor(ing.Addr)
		if err != nil {
			return nil, err
		}

		resp, err := client.(ingester_client.IngesterClient).Query(ctx, req)
		ingesterQueries.WithLabelValues(ing.Addr).Inc()
		if err != nil {
			ingesterQueryFailures.WithLabelValues(ing.Addr).Inc()
			return nil, err
		}

		return ingester_client.FromQueryResponse(resp), nil
	})
	if err != nil {
		return nil, err
	}

	// Merge the results into a single matrix.
	fpToSampleStream := map[model.Fingerprint]*model.SampleStream{}
	for _, result := range results {
		for _, ss := range result.(model.Matrix) {
			fp := ss.Metric.Fingerprint()
			mss, ok := fpToSampleStream[fp]
			if !ok {
				mss = &model.SampleStream{
					Metric: ss.Metric,
				}
				fpToSampleStream[fp] = mss
			}
			mss.Values = util.MergeSampleSets(mss.Values, ss.Values)
		}
	}
	result := model.Matrix{}
	for _, ss := range fpToSampleStream {
		result = append(result, ss)
	}

	return result, nil
}

// queryIngesterStream queries the ingesters using the new streaming API.
func (d *Distributor) queryIngesterStream(ctx context.Context, replicationSet ring.ReplicationSet, req *client.QueryRequest) (*ingester_client.QueryStreamResponse, error) {
	// Fetch samples from multiple ingesters
	results, err := replicationSet.Do(ctx, d.cfg.ExtraQueryDelay, func(ing *ring.IngesterDesc) (interface{}, error) {
		client, err := d.ingesterPool.GetClientFor(ing.Addr)
		if err != nil {
			return nil, err
		}
		ingesterQueries.WithLabelValues(ing.Addr).Inc()

		stream, err := client.(ingester_client.IngesterClient).QueryStream(ctx, req)
		if err != nil {
			ingesterQueryFailures.WithLabelValues(ing.Addr).Inc()
			return nil, err
		}
		defer stream.CloseSend() //nolint:errcheck

		result := &ingester_client.QueryStreamResponse{}
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			} else if err != nil {
				// Do not track a failure if the context was canceled.
				if !isGRPCContextCanceled(err) {
					ingesterQueryFailures.WithLabelValues(ing.Addr).Inc()
				}

				return nil, err
			}

			result.Chunkseries = append(result.Chunkseries, resp.Chunkseries...)
			result.Timeseries = append(result.Timeseries, resp.Timeseries...)
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}

	hashToChunkseries := map[model.Fingerprint]ingester_client.TimeSeriesChunk{}
	hashToTimeSeries := map[model.Fingerprint]ingester_client.TimeSeries{}

	for _, result := range results {
		response := result.(*ingester_client.QueryStreamResponse)

		// Parse any chunk series
		for _, series := range response.Chunkseries {
			hash := client.FastFingerprint(series.Labels)
			existing := hashToChunkseries[hash]
			existing.Labels = series.Labels
			existing.Chunks = append(existing.Chunks, series.Chunks...)
			hashToChunkseries[hash] = existing
		}

		// Parse any time series
		for _, series := range response.Timeseries {
			hash := client.FastFingerprint(series.Labels)
			existing := hashToTimeSeries[hash]
			existing.Labels = series.Labels
			existing.Samples = append(existing.Samples, series.Samples...)
			hashToTimeSeries[hash] = existing
		}
	}

	resp := &ingester_client.QueryStreamResponse{
		Chunkseries: make([]client.TimeSeriesChunk, 0, len(hashToChunkseries)),
		Timeseries:  make([]client.TimeSeries, 0, len(hashToTimeSeries)),
	}
	for _, series := range hashToChunkseries {
		resp.Chunkseries = append(resp.Chunkseries, series)
	}
	for _, series := range hashToTimeSeries {
		sort.Sort(byTimestamp(series.Samples))
		resp.Timeseries = append(resp.Timeseries, series)
	}

	return resp, nil
}

type byTimestamp []client.Sample

func (b byTimestamp) Len() int           { return len(b) }
func (b byTimestamp) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byTimestamp) Less(i, j int) bool { return b[i].TimestampMs < b[j].TimestampMs }

func isGRPCContextCanceled(err error) bool {
	status, ok := status.FromError(err)
	if !ok {
		return false
	}

	return status.Code() == codes.Canceled
}
