// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package prometheusremotewriteexporter implements an exporter that sends Prometheus remote write requests.
package clickhousemetricsexporter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/prometheus/prometheus/prompb"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhousemetricsexporter/base"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/consumer/pdata"
)

const (
	maxConcurrentRequests = 5
	maxBatchByteSize      = 3000000
)

// PrwExporter converts OTLP metrics to Prometheus remote write TimeSeries and sends them to a remote endpoint.
type PrwExporter struct {
	namespace       string
	externalLabels  map[string]string
	endpointURL     *url.URL
	client          *http.Client
	wg              *sync.WaitGroup
	closeChan       chan struct{}
	userAgentHeader string
	ch              base.Storage
}

// NewPrwExporter initializes a new PrwExporter instance and sets fields accordingly.
// client parameter cannot be nil.
func NewPrwExporter(namespace string, endpoint string, client *http.Client, externalLabels map[string]string, buildInfo component.BuildInfo) (*PrwExporter, error) {
	if client == nil {
		return nil, errors.New("http client cannot be nil")
	}

	sanitizedLabels, err := validateAndSanitizeExternalLabels(externalLabels)
	if err != nil {
		return nil, err
	}

	endpointURL, err := url.ParseRequestURI(endpoint)
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}

	userAgentHeader := fmt.Sprintf("%s/%s", strings.ReplaceAll(strings.ToLower(buildInfo.Description), " ", "-"), buildInfo.Version)

	params := &ClickHouseParams{
		DSN:                  endpoint,
		DropDatabase:         false,
		MaxOpenConns:         75,
		MaxTimeSeriesInQuery: 50,
	}
	ch, err := NewClickHouse(params)
	if err != nil {
		zap.S().Error("couldn't create instance of clickhouse")
	}

	return &PrwExporter{
		namespace:       namespace,
		externalLabels:  sanitizedLabels,
		endpointURL:     endpointURL,
		client:          client,
		wg:              new(sync.WaitGroup),
		closeChan:       make(chan struct{}),
		userAgentHeader: userAgentHeader,
		ch:              ch,
	}, nil
}

// Shutdown stops the exporter from accepting incoming calls(and return error), and wait for current export operations
// to finish before returning
func (prwe *PrwExporter) Shutdown(context.Context) error {
	close(prwe.closeChan)
	prwe.wg.Wait()
	return nil
}

// PushMetrics converts metrics to Prometheus remote write TimeSeries and send to remote endpoint. It maintain a map of
// TimeSeries, validates and handles each individual metric, adding the converted TimeSeries to the map, and finally
// exports the map.
func (prwe *PrwExporter) PushMetrics(ctx context.Context, md pdata.Metrics) error {
	prwe.wg.Add(1)
	defer prwe.wg.Done()

	select {
	case <-prwe.closeChan:
		return errors.New("shutdown has been called")
	default:
		tsMap := map[string]*prompb.TimeSeries{}
		dropped := 0
		var errs []error
		resourceMetricsSlice := md.ResourceMetrics()
		for i := 0; i < resourceMetricsSlice.Len(); i++ {
			resourceMetrics := resourceMetricsSlice.At(i)
			resource := resourceMetrics.Resource()
			instrumentationLibraryMetricsSlice := resourceMetrics.InstrumentationLibraryMetrics()
			// TODO: add resource attributes as labels, probably in next PR
			for j := 0; j < instrumentationLibraryMetricsSlice.Len(); j++ {
				instrumentationLibraryMetrics := instrumentationLibraryMetricsSlice.At(j)
				metricSlice := instrumentationLibraryMetrics.Metrics()

				// TODO: decide if instrumentation library information should be exported as labels
				for k := 0; k < metricSlice.Len(); k++ {
					metric := metricSlice.At(k)

					// check for valid type and temporality combination and for matching data field and type
					if ok := validateMetrics(metric); !ok {
						dropped++
						errs = append(errs, consumererror.Permanent(errors.New("invalid temporality and type combination")))
						continue
					}

					// handle individual metric based on type
					switch metric.DataType() {
					case pdata.MetricDataTypeDoubleSum, pdata.MetricDataTypeIntSum, pdata.MetricDataTypeDoubleGauge, pdata.MetricDataTypeIntGauge:
						if err := prwe.handleScalarMetric(tsMap, resource, metric); err != nil {
							dropped++
							errs = append(errs, consumererror.Permanent(err))
						}
					case pdata.MetricDataTypeHistogram, pdata.MetricDataTypeIntHistogram:
						if err := prwe.handleHistogramMetric(tsMap, resource, metric); err != nil {
							dropped++
							errs = append(errs, consumererror.Permanent(err))
						}
					case pdata.MetricDataTypeSummary:
						if err := prwe.handleSummaryMetric(tsMap, resource, metric); err != nil {
							dropped++
							errs = append(errs, consumererror.Permanent(err))
						}
					default:
						dropped++
						errs = append(errs, consumererror.Permanent(errors.New("unsupported metric type")))
					}
				}
			}
		}

		if exportErrors := prwe.export(ctx, tsMap); len(exportErrors) != 0 {
			dropped = md.MetricCount()
			errs = append(errs, exportErrors...)
		}

		if dropped != 0 {
			return consumererror.Combine(errs)
		}

		return nil
	}
}

func validateAndSanitizeExternalLabels(externalLabels map[string]string) (map[string]string, error) {
	sanitizedLabels := make(map[string]string)
	for key, value := range externalLabels {
		if key == "" || value == "" {
			return nil, fmt.Errorf("prometheus remote write: external labels configuration contains an empty key or value")
		}

		// Sanitize label keys to meet Prometheus Requirements
		if len(key) > 2 && key[:2] == "__" {
			key = "__" + sanitize(key[2:])
		} else {
			key = sanitize(key)
		}
		sanitizedLabels[key] = value
	}

	return sanitizedLabels, nil
}

// handleScalarMetric processes data points in a single OTLP scalar metric by adding the each point as a Sample into
// its corresponding TimeSeries in tsMap.
// tsMap and metric cannot be nil, and metric must have a non-nil descriptor
func (prwe *PrwExporter) handleScalarMetric(tsMap map[string]*prompb.TimeSeries, resource pdata.Resource, metric pdata.Metric) error {
	switch metric.DataType() {
	// int points
	case pdata.MetricDataTypeDoubleGauge:
		dataPoints := metric.DoubleGauge().DataPoints()
		if dataPoints.Len() == 0 {
			return fmt.Errorf("empty data points. %s is dropped", metric.Name())
		}

		for i := 0; i < dataPoints.Len(); i++ {
			addSingleDoubleDataPoint(dataPoints.At(i), resource, metric, prwe.namespace, tsMap, prwe.externalLabels)
		}
	case pdata.MetricDataTypeIntGauge:
		dataPoints := metric.IntGauge().DataPoints()
		if dataPoints.Len() == 0 {
			return fmt.Errorf("empty data points. %s is dropped", metric.Name())
		}
		for i := 0; i < dataPoints.Len(); i++ {
			addSingleIntDataPoint(dataPoints.At(i), resource, metric, prwe.namespace, tsMap, prwe.externalLabels)
		}
	case pdata.MetricDataTypeDoubleSum:
		dataPoints := metric.DoubleSum().DataPoints()
		if dataPoints.Len() == 0 {
			return fmt.Errorf("empty data points. %s is dropped", metric.Name())
		}
		for i := 0; i < dataPoints.Len(); i++ {
			addSingleDoubleDataPoint(dataPoints.At(i), resource, metric, prwe.namespace, tsMap, prwe.externalLabels)

		}
	case pdata.MetricDataTypeIntSum:
		dataPoints := metric.IntSum().DataPoints()
		if dataPoints.Len() == 0 {
			return fmt.Errorf("empty data points. %s is dropped", metric.Name())
		}
		for i := 0; i < dataPoints.Len(); i++ {
			addSingleIntDataPoint(dataPoints.At(i), resource, metric, prwe.namespace, tsMap, prwe.externalLabels)
		}
	}
	return nil
}

// handleHistogramMetric processes data points in a single OTLP histogram metric by mapping the sum, count and each
// bucket of every data point as a Sample, and adding each Sample to its corresponding TimeSeries.
// tsMap and metric cannot be nil.
func (prwe *PrwExporter) handleHistogramMetric(tsMap map[string]*prompb.TimeSeries, resource pdata.Resource, metric pdata.Metric) error {
	switch metric.DataType() {
	case pdata.MetricDataTypeIntHistogram:
		dataPoints := metric.IntHistogram().DataPoints()
		if dataPoints.Len() == 0 {
			return fmt.Errorf("empty data points. %s is dropped", metric.Name())
		}
		for i := 0; i < dataPoints.Len(); i++ {
			addSingleIntHistogramDataPoint(dataPoints.At(i), resource, metric, prwe.namespace, tsMap, prwe.externalLabels)
		}
	case pdata.MetricDataTypeHistogram:
		dataPoints := metric.Histogram().DataPoints()
		if dataPoints.Len() == 0 {
			return fmt.Errorf("empty data points. %s is dropped", metric.Name())
		}
		for i := 0; i < dataPoints.Len(); i++ {
			addSingleDoubleHistogramDataPoint(dataPoints.At(i), resource, metric, prwe.namespace, tsMap, prwe.externalLabels)
		}
	}
	return nil
}

// handleSummaryMetric processes data points in a single OTLP summary metric by mapping the sum, count and each
// quantile of every data point as a Sample, and adding each Sample to its corresponding TimeSeries.
// tsMap and metric cannot be nil.
func (prwe *PrwExporter) handleSummaryMetric(tsMap map[string]*prompb.TimeSeries, resource pdata.Resource, metric pdata.Metric) error {
	dataPoints := metric.Summary().DataPoints()
	if dataPoints.Len() == 0 {
		return fmt.Errorf("empty data points. %s is dropped", metric.Name())
	}
	for i := 0; i < dataPoints.Len(); i++ {
		addSingleDoubleSummaryDataPoint(dataPoints.At(i), resource, metric, prwe.namespace, tsMap, prwe.externalLabels)
	}
	return nil
}

// export sends a Snappy-compressed WriteRequest containing TimeSeries to a remote write endpoint in order
func (prwe *PrwExporter) export(ctx context.Context, tsMap map[string]*prompb.TimeSeries) []error {
	var errs []error
	// Calls the helper function to convert and batch the TsMap to the desired format
	requests, err := batchTimeSeries(tsMap, maxBatchByteSize)
	if err != nil {
		errs = append(errs, consumererror.Permanent(err))
		return errs
	}

	for _, data := range requests {
		err := prwe.ch.Write(ctx, data)
		errs = append(errs, err)
	}

	return errs
}
