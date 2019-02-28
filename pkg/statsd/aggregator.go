package statsd

import (
	"context"
	"github.com/emirpasic/gods/maps/treemap"
	"github.com/emirpasic/gods/utils"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/atlassian/gostatsd"
	"github.com/atlassian/gostatsd/pkg/stats"
)

// percentStruct is a cache of percentile names to avoid creating them for each timer.
type percentStruct struct {
	count      string
	mean       string
	sum        string
	sumSquares string
	upper      string
	lower      string
}

// MetricAggregator aggregates metrics.
type MetricAggregator struct {
	metricsReceived   uint64
	expiryInterval    time.Duration // How often to expire metrics
	percentThresholds map[float64]percentStruct
	now               func() time.Time // Returns current time. Useful for testing.
	statser           stats.Statser
	disabledSubtypes  gostatsd.TimerSubtypes
	metricMap         *gostatsd.MetricMap
}

// NewMetricAggregator creates a new MetricAggregator object.
func NewMetricAggregator(percentThresholds []float64, expiryInterval time.Duration, disabled gostatsd.TimerSubtypes) *MetricAggregator {
	a := MetricAggregator{
		expiryInterval:    expiryInterval,
		percentThresholds: make(map[float64]percentStruct, len(percentThresholds)),
		now:               time.Now,
		statser:           stats.NewNullStatser(), // Will probably be replaced via RunMetrics
		metricMap:         gostatsd.NewMetricMap(),
		disabledSubtypes:  disabled,
	}
	for _, pct := range percentThresholds {
		sPct := strconv.Itoa(int(pct))
		a.percentThresholds[pct] = percentStruct{
			count:      "count_" + sPct,
			mean:       "mean_" + sPct,
			sum:        "sum_" + sPct,
			sumSquares: "sum_squares_" + sPct,
			upper:      "upper_" + sPct,
			lower:      "lower_" + sPct,
		}
	}
	return &a
}

// round rounds a number to its nearest integer value.
// poor man's math.Round(x) = math.Floor(x + 0.5).
func round(v float64) float64 {
	return math.Floor(v + 0.5)
}

// Flush prepares the contents of a MetricAggregator for sending via the Sender.
func (a *MetricAggregator) Flush(flushInterval time.Duration) {
	a.statser.Gauge("aggregator.metrics_received", float64(a.metricsReceived), nil)

	flushInSeconds := float64(flushInterval) / float64(time.Second)

	a.metricMap.Counters.Each(func(key, tagsKey string, counter gostatsd.Counter) {
		counter.PerSecond = float64(counter.Value) / flushInSeconds
		a.metricMap.Counters[key][tagsKey] = counter
	})

	a.metricMap.Timers.Each(func(key, tagsKey string, timer gostatsd.Timer) {
		if contains(timer.Tags, "percentiles:true") {
			fancyPipeline(timer, a, flushInSeconds, key, tagsKey)
		} else {
			defaultPipeline(timer, a, flushInSeconds, key, tagsKey)
		}
	})
}

func fancyPipeline(timer gostatsd.Timer, a *MetricAggregator, flushInSeconds float64, key string, tagsKey string) {
	buckets := [12]float64{20.0,30.0,40.0,50.0,75.0,100.0,200.0,500.0,1000.0,2000.0,5000.0}

	var valuesMap = *treemap.NewWith(utils.Float64Comparator)
	for _, value := range buckets {
		valuesMap.Put(value, 0)
	}

	for _, value := range timer.Values {
		foundKey, foundValue := valuesMap.Ceiling(value)

		if foundKey == value {
			foundKey, foundValue = valuesMap.Ceiling(value+0.000000001)
		}

		valuesMap.Put(foundKey, foundValue.(int)+1)
	}

	var result = make(map[float64]int)

	valuesMap.Each(func(key interface{}, value interface{}) {
		result[key.(float64)] = value.(int)
	})

	timer.Buckets = result
	a.metricMap.Timers[key][tagsKey] = timer
}

// Contains tells whether a contains x.
func contains(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}

func defaultPipeline(timer gostatsd.Timer, a *MetricAggregator, flushInSeconds float64, key string, tagsKey string) {
	if count := len(timer.Values); count > 0 {
		sort.Float64s(timer.Values)
		timer.Min = timer.Values[0]
		timer.Max = timer.Values[count-1]
		n := len(timer.Values)
		count := float64(n)

		cumulativeValues := make([]float64, n)
		cumulSumSquaresValues := make([]float64, n)
		cumulativeValues[0] = timer.Min
		cumulSumSquaresValues[0] = timer.Min * timer.Min
		for i := 1; i < n; i++ {
			cumulativeValues[i] = timer.Values[i] + cumulativeValues[i-1]
			cumulSumSquaresValues[i] = timer.Values[i]*timer.Values[i] + cumulSumSquaresValues[i-1]
		}

		var sumSquares = timer.Min * timer.Min
		var mean = timer.Min
		var sum = timer.Min
		var thresholdBoundary = timer.Max

		for pct, pctStruct := range a.percentThresholds {
			numInThreshold := n
			if n > 1 {
				numInThreshold = int(round(math.Abs(pct) / 100 * count))
				if numInThreshold == 0 {
					continue
				}
				if pct > 0 {
					thresholdBoundary = timer.Values[numInThreshold-1]
					sum = cumulativeValues[numInThreshold-1]
					sumSquares = cumulSumSquaresValues[numInThreshold-1]
				} else {
					thresholdBoundary = timer.Values[n-numInThreshold]
					sum = cumulativeValues[n-1] - cumulativeValues[n-numInThreshold-1]
					sumSquares = cumulSumSquaresValues[n-1] - cumulSumSquaresValues[n-numInThreshold-1]
				}
				mean = sum / float64(numInThreshold)
			}

			if !a.disabledSubtypes.CountPct {
				timer.Percentiles.Set(pctStruct.count, float64(numInThreshold))
			}
			if !a.disabledSubtypes.MeanPct {
				timer.Percentiles.Set(pctStruct.mean, mean)
			}
			if !a.disabledSubtypes.SumPct {
				timer.Percentiles.Set(pctStruct.sum, sum)
			}
			if !a.disabledSubtypes.SumSquaresPct {
				timer.Percentiles.Set(pctStruct.sumSquares, sumSquares)
			}
			if pct > 0 {
				if !a.disabledSubtypes.UpperPct {
					timer.Percentiles.Set(pctStruct.upper, thresholdBoundary)
				}
			} else {
				if !a.disabledSubtypes.LowerPct {
					timer.Percentiles.Set(pctStruct.lower, thresholdBoundary)
				}
			}
		}

		sum = cumulativeValues[n-1]
		sumSquares = cumulSumSquaresValues[n-1]
		mean = sum / count

		var sumOfDiffs float64
		for i := 0; i < n; i++ {
			sumOfDiffs += (timer.Values[i] - mean) * (timer.Values[i] - mean)
		}

		mid := int(math.Floor(count / 2))
		if math.Mod(count, 2) == 0 {
			timer.Median = (timer.Values[mid-1] + timer.Values[mid]) / 2
		} else {
			timer.Median = timer.Values[mid]
		}

		timer.Mean = mean
		timer.StdDev = math.Sqrt(sumOfDiffs / count)
		timer.Sum = sum
		timer.SumSquares = sumSquares

		timer.Count = int(round(timer.SampledCount))
		timer.PerSecond = timer.SampledCount / flushInSeconds

		a.metricMap.Timers[key][tagsKey] = timer
	} else {
		timer.Count = 0
		timer.SampledCount = 0
		timer.PerSecond = 0
	}
}

func (a *MetricAggregator) RunMetrics(ctx context.Context, statser stats.Statser) {
	a.statser = statser
}

func (a *MetricAggregator) Process(f ProcessFunc) {
	f(a.metricMap)
}

func (a *MetricAggregator) isExpired(now, ts gostatsd.Nanotime) bool {
	return a.expiryInterval != 0 && time.Duration(now-ts) > a.expiryInterval
}

func deleteMetric(key, tagsKey string, metrics gostatsd.AggregatedMetrics) {
	metrics.DeleteChild(key, tagsKey)
	if !metrics.HasChildren(key) {
		metrics.Delete(key)
	}
}

// Reset clears the contents of a MetricAggregator.
func (a *MetricAggregator) Reset() {
	a.metricsReceived = 0
	nowNano := gostatsd.Nanotime(a.now().UnixNano())

	a.metricMap.Counters.Each(func(key, tagsKey string, counter gostatsd.Counter) {
		if a.isExpired(nowNano, counter.Timestamp) {
			deleteMetric(key, tagsKey, a.metricMap.Counters)
		} else {
			a.metricMap.Counters[key][tagsKey] = gostatsd.Counter{
				Timestamp: counter.Timestamp,
				Hostname:  counter.Hostname,
				Tags:      counter.Tags,
			}
		}
	})

	a.metricMap.Timers.Each(func(key, tagsKey string, timer gostatsd.Timer) {
		if a.isExpired(nowNano, timer.Timestamp) {
			deleteMetric(key, tagsKey, a.metricMap.Timers)
		} else {
			a.metricMap.Timers[key][tagsKey] = gostatsd.Timer{
				Timestamp: timer.Timestamp,
				Hostname:  timer.Hostname,
				Tags:      timer.Tags,
				Values:    timer.Values[:0],
			}
		}
	})

	a.metricMap.Gauges.Each(func(key, tagsKey string, gauge gostatsd.Gauge) {
		if a.isExpired(nowNano, gauge.Timestamp) {
			deleteMetric(key, tagsKey, a.metricMap.Gauges)
		}
		// No reset for gauges, they keep the last value until expiration
	})

	a.metricMap.Sets.Each(func(key, tagsKey string, set gostatsd.Set) {
		if a.isExpired(nowNano, set.Timestamp) {
			deleteMetric(key, tagsKey, a.metricMap.Sets)
		} else {
			a.metricMap.Sets[key][tagsKey] = gostatsd.Set{
				Values:    make(map[string]struct{}),
				Timestamp: set.Timestamp,
				Hostname:  set.Hostname,
				Tags:      set.Tags,
			}
		}
	})
}

// Receive aggregates an incoming metric.
func (a *MetricAggregator) Receive(m *gostatsd.Metric, now time.Time) {
	a.metricsReceived++
	a.metricMap.Receive(m, now)
}
