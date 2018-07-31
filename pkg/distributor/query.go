package distributor

import (
	"context"
	"io"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql"

	"github.com/weaveworks/common/instrument"
	"github.com/weaveworks/common/user"
	"github.com/weaveworks/cortex/pkg/ingester/client"
	ingester_client "github.com/weaveworks/cortex/pkg/ingester/client"
	"github.com/weaveworks/cortex/pkg/ring"
	"github.com/weaveworks/cortex/pkg/util"
	"github.com/weaveworks/cortex/pkg/util/extract"
)

// Query multiple ingesters and returns a Matrix of samples.
func (d *Distributor) Query(ctx context.Context, from, to model.Time, matchers ...*labels.Matcher) (model.Matrix, error) {
	var matrix model.Matrix
	err := instrument.TimeRequestHistogram(ctx, "Distributor.Query", queryDuration, func(ctx context.Context) error {
		replicationSet, req, err := d.queryPrep(ctx, from, to, matchers...)
		if err != nil {
			return promql.ErrStorage(err)
		}

		matrix, err = d.queryIngesters(ctx, replicationSet, req)
		if err != nil {
			return promql.ErrStorage(err)
		}
		return nil
	})
	return matrix, err
}

// QueryStream multiple ingesters via the streaming interface and returns big ol' set of chunks.
func (d *Distributor) QueryStream(ctx context.Context, from, to model.Time, matchers ...*labels.Matcher) ([]*client.QueryStreamResponse, error) {
	var result []*client.QueryStreamResponse
	err := instrument.TimeRequestHistogram(ctx, "Distributor.Query", queryDuration, func(ctx context.Context) error {
		replicationSet, req, err := d.queryPrep(ctx, from, to, matchers...)
		if err != nil {
			return promql.ErrStorage(err)
		}

		result, err = d.queryIngesterStream(ctx, replicationSet, req)
		if err != nil {
			return promql.ErrStorage(err)
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
		replicationSet, err = d.ring.Get(shardByMetricName(userID, []byte(metricNameMatcher.Value)), ring.Read)
	} else {
		replicationSet, err = d.ring.GetAll()
	}
	return replicationSet, req, err
}

// queryIngesters queries the ingesters via the older, sample-based API.
func (d *Distributor) queryIngesters(ctx context.Context, replicationSet ring.ReplicationSet, req *client.QueryRequest) (model.Matrix, error) {
	// Fetch samples from multiple ingesters in parallel, using the replicationSet
	// to deal with consistency.
	results, err := replicationSet.Do(ctx, func(ing *ring.IngesterDesc) (interface{}, error) {
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
func (d *Distributor) queryIngesterStream(ctx context.Context, replicationSet ring.ReplicationSet, req *client.QueryRequest) ([]*client.QueryStreamResponse, error) {
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	}

	// Fetch samples from multiple ingesters
	results, err := replicationSet.Do(ctx, func(ing *ring.IngesterDesc) (interface{}, error) {
		client, err := d.ingesterPool.GetClientFor(ing.Addr)
		if err != nil {
			return nil, err
		}

		stream, err := client.(ingester_client.IngesterClient).QueryStream(ctx, req)
		ingesterQueries.WithLabelValues(ing.Addr).Inc()
		if err != nil {
			ingesterQueryFailures.WithLabelValues(ing.Addr).Inc()
			return nil, err
		}

		var result []*ingester_client.QueryStreamResponse
		for {
			series, err := stream.Recv()
			if err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
			result = append(result, series)
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}

	hashToSeries := map[uint32]*ingester_client.QueryStreamResponse{}
	for _, result := range results {
		for i := range result.([]*ingester_client.QueryStreamResponse) {
			response := result.([]*ingester_client.QueryStreamResponse)[i]

			hash, err := shardByAllLabels(userID, response.Labels)
			if err != nil {
				return nil, err
			}

			series, ok := hashToSeries[hash]
			if !ok {
				hashToSeries[hash] = response
				continue
			}

			series.Chunks = append(series.Chunks, response.Chunks...)
		}
	}
	result := make([]*client.QueryStreamResponse, 0, len(hashToSeries))
	for _, series := range hashToSeries {
		result = append(result, series)
	}

	return result, nil
}
