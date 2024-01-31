package aggregators

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	semconv "go.opentelemetry.io/collector/semconv/v1.5.0"

	"github.com/elastic/apm-aggregation/aggregationpb"
	"github.com/elastic/apm-data/model/modelpb"
)

var (
	serviceNameInvalidRegexp = regexp.MustCompile("[^a-zA-Z0-9 _-]")
)

const (
	keywordLength = 1024
)

// truncate returns s truncated at n runes, and the number of runes in the resulting string (<= n).
func truncate(s string) string {
	var j int
	for i := range s {
		if j == keywordLength {
			return s[:i]
		}
		j++
	}
	return s
}

func cleanServiceName(name string) string {
	return serviceNameInvalidRegexp.ReplaceAllString(truncate(name), "_")
}

func (a *Aggregator) aggregateOTelLogs(
	ctx context.Context,
	cmk CombinedMetricsKey,
	logs plog.Logs,
) (int, error) {
	var totalBytesIn int
	aggregateFunc := func(k CombinedMetricsKey, m *aggregationpb.CombinedMetrics) error {
		bytesIn, err := a.aggregate(ctx, k, m)
		totalBytesIn += bytesIn
		return err
	}

	resourceLogs := logs.ResourceLogs()
	for i := 0; i < resourceLogs.Len(); i++ {
		resourceLog := resourceLogs.At(i)
		attrs := resourceLog.Resource().Attributes()

		var (
			serviceName         = "unknown"
			serviceEnvironment  string
			agentName           = "otlp"
			serviceLanguageName string
		)

		attrs.Range(func(k string, v pcommon.Value) bool {
			switch k {
			// service.*
			case semconv.AttributeServiceName:
				serviceName = cleanServiceName(v.Str())

			// deployment.*
			case semconv.AttributeDeploymentEnvironment:
				serviceEnvironment = truncate(v.Str())

			// telemetry.sdk.*
			case semconv.AttributeTelemetrySDKName:
				agentName = truncate(v.Str())
			case semconv.AttributeTelemetrySDKLanguage:
				serviceLanguageName = truncate(v.Str())
			}
			return true
		})

		scopeLogs := resourceLog.ScopeLogs()
		for j := 0; j < scopeLogs.Len(); j++ {
			scopeLog := scopeLogs.At(j)
			logRecords := scopeLog.LogRecords()
			for k := 0; k < logRecords.Len(); k++ {
				err := otelLogsToCombinedMetrics(logRecords.At(k),
					serviceName, serviceEnvironment, serviceLanguageName, agentName,
					cmk, a.cfg.Partitions, aggregateFunc)
				if err != nil {
					return 0, fmt.Errorf("failed to aggregate combined metrics: %w", err)
				}
			}
		}
	}
	return totalBytesIn, nil
}

func otelLogsToCombinedMetrics(
	record plog.LogRecord,
	serviceName, serviceEnvironment, serviceLanguageName, agentName string,
	unpartitionedKey CombinedMetricsKey,
	partitions uint16,
	callback func(CombinedMetricsKey, *aggregationpb.CombinedMetrics) error,
) error {
	// FIXME(carsonip): no global labels support yet. Ideally, all service level labels are global as implemented in
	// apm-data conversion code.

	//globalLabels, err := marshalEventGlobalLabels(e)
	//if err != nil {
	//	return fmt.Errorf("failed to marshal global labels: %w", err)
	//}

	ts := record.Timestamp().AsTime()

	pmb := getPartitionedMetricsBuilder(
		aggregationpb.ServiceAggregationKey{
			Timestamp: modelpb.FromTime(
				ts.Truncate(unpartitionedKey.Interval),
			),
			ServiceName:         serviceName,
			ServiceEnvironment:  serviceEnvironment,
			ServiceLanguageName: serviceLanguageName,
			AgentName:           agentName,
			GlobalLabelsStr:     nil,
		},
		partitions,
	)
	defer pmb.release()

	pmb.processOTelLogs()
	if len(pmb.builders) == 0 {
		// This is unexpected state as any APMEvent must result in atleast the
		// service summary metric. If such a state happens then it would indicate
		// a bug in `processEvent`.
		return fmt.Errorf("service summary metric must be produced for any event")
	}

	// Approximate events total by uniformly distributing the events total
	// amongst the partitioned key values.
	pmb.combinedMetrics.EventsTotal = 1 / float64(len(pmb.builders))
	pmb.combinedMetrics.YoungestEventTimestamp = modelpb.FromTime(ts) // FIXME(carsonip): use observed timestamp

	var errs []error
	for _, mb := range pmb.builders {
		key := unpartitionedKey
		key.PartitionID = mb.partition
		pmb.serviceMetrics.TransactionMetrics = mb.keyedTransactionMetricsSlice
		pmb.serviceMetrics.ServiceTransactionMetrics = mb.keyedServiceTransactionMetricsSlice
		pmb.serviceMetrics.SpanMetrics = mb.keyedSpanMetricsSlice
		if err := callback(key, &pmb.combinedMetrics); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed while executing callback: %w", errors.Join(errs...))
	}
	return nil
}
