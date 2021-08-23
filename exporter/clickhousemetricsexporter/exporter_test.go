// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package clickhousemetricsexporter

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/internal/testdata"
)

// Test_ NewPrwExporter checks that a new exporter instance with non-nil fields is initialized
func Test_NewPrwExporter(t *testing.T) {
	cfg := &Config{
		ExporterSettings:   config.NewExporterSettings(config.NewID(typeStr)),
		TimeoutSettings:    exporterhelper.TimeoutSettings{},
		QueueSettings:      exporterhelper.QueueSettings{},
		RetrySettings:      exporterhelper.RetrySettings{},
		Namespace:          "",
		ExternalLabels:     map[string]string{},
		HTTPClientSettings: confighttp.HTTPClientSettings{Endpoint: ""},
	}
	buildInfo := component.BuildInfo{
		Description: "OpenTelemetry Collector",
		Version:     "1.0",
	}

	tests := []struct {
		name           string
		config         *Config
		namespace      string
		endpoint       string
		externalLabels map[string]string
		client         *http.Client
		returnError    bool
		buildInfo      component.BuildInfo
	}{
		{
			"invalid_URL",
			cfg,
			"test",
			"invalid URL",
			map[string]string{"Key1": "Val1"},
			http.DefaultClient,
			true,
			buildInfo,
		},
		{
			"nil_client",
			cfg,
			"test",
			"http://some.url:9411/api/prom/push",
			map[string]string{"Key1": "Val1"},
			nil,
			true,
			buildInfo,
		},
		{
			"invalid_labels_case",
			cfg,
			"test",
			"http://some.url:9411/api/prom/push",
			map[string]string{"Key1": ""},
			http.DefaultClient,
			true,
			buildInfo,
		},
		{
			"success_case",
			cfg,
			"test",
			"http://some.url:9411/api/prom/push",
			map[string]string{"Key1": "Val1"},
			http.DefaultClient,
			false,
			buildInfo,
		},
		{
			"success_case_no_labels",
			cfg,
			"test",
			"http://some.url:9411/api/prom/push",
			map[string]string{},
			http.DefaultClient,
			false,
			buildInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prwe, err := NewPrwExporter(tt.namespace, tt.endpoint, tt.client, tt.externalLabels, tt.buildInfo)
			if tt.returnError {
				assert.Error(t, err)
				return
			}
			require.NotNil(t, prwe)
			assert.NotNil(t, prwe.namespace)
			assert.NotNil(t, prwe.endpointURL)
			assert.NotNil(t, prwe.externalLabels)
			assert.NotNil(t, prwe.client)
			assert.NotNil(t, prwe.closeChan)
			assert.NotNil(t, prwe.wg)
			assert.NotNil(t, prwe.userAgentHeader)
		})
	}
}

// Test_Shutdown checks after Shutdown is called, incoming calls to PushMetrics return error.
func Test_Shutdown(t *testing.T) {
	prwe := &PrwExporter{
		wg:        new(sync.WaitGroup),
		closeChan: make(chan struct{}),
	}
	wg := new(sync.WaitGroup)
	err := prwe.Shutdown(context.Background())
	require.NoError(t, err)
	errChan := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errChan <- prwe.PushMetrics(context.Background(), testdata.GenerateMetricsEmpty())
		}()
	}
	wg.Wait()
	close(errChan)
	for ok := range errChan {
		assert.Error(t, ok)
	}
}

// Test whether or not the Server receives the correct TimeSeries.
// Currently considering making this test an iterative for loop of multiple TimeSeries much akin to Test_PushMetrics
func Test_export(t *testing.T) {
	// First we will instantiate a dummy TimeSeries instance to pass into both the export call and compare the http request
	labels := getPromLabels(label11, value11, label12, value12, label21, value21, label22, value22)
	sample1 := getSample(floatVal1, msTime1)
	sample2 := getSample(floatVal2, msTime2)
	ts1 := getTimeSeries(labels, sample1, sample2)
	handleFunc := func(w http.ResponseWriter, r *http.Request, code int) {
		// The following is a handler function that reads the sent httpRequest, unmarshal, and checks if the WriteRequest
		// preserves the TimeSeries data correctly
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		require.NotNil(t, body)
		// Receives the http requests and unzip, unmarshals, and extracts TimeSeries
		assert.Equal(t, "0.1.0", r.Header.Get("X-Prometheus-Remote-Write-Version"))
		assert.Equal(t, "snappy", r.Header.Get("Content-Encoding"))
		assert.Equal(t, "opentelemetry-collector/1.0", r.Header.Get("User-Agent"))
		writeReq := &prompb.WriteRequest{}
		unzipped := []byte{}

		dest, err := snappy.Decode(unzipped, body)
		require.NoError(t, err)

		ok := proto.Unmarshal(dest, writeReq)
		require.NoError(t, ok)

		assert.EqualValues(t, 1, len(writeReq.Timeseries))
		require.NotNil(t, writeReq.GetTimeseries())
		assert.Equal(t, *ts1, writeReq.GetTimeseries()[0])
		w.WriteHeader(code)
	}

	// Create in test table format to check if different HTTP response codes or server errors
	// are properly identified
	tests := []struct {
		name             string
		ts               prompb.TimeSeries
		serverUp         bool
		httpResponseCode int
		returnError      bool
	}{
		{"success_case",
			*ts1,
			true,
			http.StatusAccepted,
			false,
		},
		{
			"server_no_response_case",
			*ts1,
			false,
			http.StatusAccepted,
			true,
		}, {
			"error_status_code_case",
			*ts1,
			true,
			http.StatusForbidden,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if handleFunc != nil {
					handleFunc(w, r, tt.httpResponseCode)
				}
			}))
			defer server.Close()
			serverURL, uErr := url.Parse(server.URL)
			assert.NoError(t, uErr)
			if !tt.serverUp {
				server.Close()
			}
			errs := runExportPipeline(ts1, serverURL)
			if tt.returnError {
				assert.Error(t, errs[0])
				return
			}
			assert.Len(t, errs, 0)
		})
	}
}

func runExportPipeline(ts *prompb.TimeSeries, endpoint *url.URL) []error {
	var errs []error

	// First we will construct a TimeSeries array from the testutils package
	testmap := make(map[string]*prompb.TimeSeries)
	testmap["test"] = ts

	HTTPClient := http.DefaultClient

	buildInfo := component.BuildInfo{
		Description: "OpenTelemetry Collector",
		Version:     "1.0",
	}
	// after this, instantiate a CortexExporter with the current HTTP client and endpoint set to passed in endpoint
	prwe, err := NewPrwExporter("test", endpoint.String(), HTTPClient, map[string]string{}, buildInfo)
	if err != nil {
		errs = append(errs, err)
		return errs
	}
	errs = append(errs, prwe.export(context.Background(), testmap)...)
	return errs
}

// Test_PushMetrics checks the number of TimeSeries received by server and the number of metrics dropped is the same as
// expected
func Test_PushMetrics(t *testing.T) {

	invalidTypeBatch := testdata.GenerateMetricsMetricTypeInvalid()

	// success cases
	intSumBatch := testdata.GenerateMetricsManyMetricsSameResource(10)

	doubleSumBatch := getMetricsFromMetricList(validMetrics1[validDoubleSum], validMetrics2[validDoubleSum])

	intGaugeBatch := getMetricsFromMetricList(validMetrics1[validIntGauge], validMetrics2[validIntGauge])

	doubleGaugeBatch := getMetricsFromMetricList(validMetrics1[validDoubleGauge], validMetrics2[validDoubleGauge])

	intHistogramBatch := getMetricsFromMetricList(validMetrics1[validIntHistogram], validMetrics2[validIntHistogram])

	histogramBatch := getMetricsFromMetricList(validMetrics1[validHistogram], validMetrics2[validHistogram])

	summaryBatch := getMetricsFromMetricList(validMetrics1[validSummary], validMetrics2[validSummary])

	// len(BucketCount) > len(ExplicitBounds)
	unmatchedBoundBucketIntHistBatch := getMetricsFromMetricList(validMetrics2[unmatchedBoundBucketIntHist])

	unmatchedBoundBucketHistBatch := getMetricsFromMetricList(validMetrics2[unmatchedBoundBucketHist])

	// fail cases
	emptyIntGaugeBatch := getMetricsFromMetricList(invalidMetrics[emptyIntGauge])

	emptyDoubleGaugeBatch := getMetricsFromMetricList(invalidMetrics[emptyDoubleGauge])

	emptyCumulativeIntSumBatch := getMetricsFromMetricList(invalidMetrics[emptyCumulativeIntSum])

	emptyCumulativeDoubleSumBatch := getMetricsFromMetricList(invalidMetrics[emptyCumulativeDoubleSum])

	emptyCumulativeIntHistogramBatch := getMetricsFromMetricList(invalidMetrics[emptyCumulativeIntHistogram])

	emptyCumulativeHistogramBatch := getMetricsFromMetricList(invalidMetrics[emptyCumulativeHistogram])

	emptyCumulativeSummaryBatch := getMetricsFromMetricList(invalidMetrics[emptySummary])

	checkFunc := func(t *testing.T, r *http.Request, expected int) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}

		buf := make([]byte, len(body))
		dest, err := snappy.Decode(buf, body)
		assert.Equal(t, "0.1.0", r.Header.Get("x-prometheus-remote-write-version"))
		assert.Equal(t, "snappy", r.Header.Get("content-encoding"))
		assert.Equal(t, "opentelemetry-collector/1.0", r.Header.Get("User-Agent"))
		assert.NotNil(t, r.Header.Get("tenant-id"))
		require.NoError(t, err)
		wr := &prompb.WriteRequest{}
		ok := proto.Unmarshal(dest, wr)
		require.Nil(t, ok)
		assert.EqualValues(t, expected, len(wr.Timeseries))
	}

	tests := []struct {
		name               string
		md                 *pdata.Metrics
		reqTestFunc        func(t *testing.T, r *http.Request, expected int)
		expectedTimeSeries int
		httpResponseCode   int
		returnErr          bool
	}{
		{
			"invalid_type_case",
			&invalidTypeBatch,
			nil,
			0,
			http.StatusAccepted,
			true,
		},
		{
			"intSum_case",
			&intSumBatch,
			checkFunc,
			2,
			http.StatusAccepted,
			false,
		},
		{
			"doubleSum_case",
			&doubleSumBatch,
			checkFunc,
			2,
			http.StatusAccepted,
			false,
		},
		{
			"doubleGauge_case",
			&doubleGaugeBatch,
			checkFunc,
			2,
			http.StatusAccepted,
			false,
		},
		{
			"intGauge_case",
			&intGaugeBatch,
			checkFunc,
			2,
			http.StatusAccepted,
			false,
		},
		{
			"intHistogram_case",
			&intHistogramBatch,
			checkFunc,
			12,
			http.StatusAccepted,
			false,
		},
		{
			"histogram_case",
			&histogramBatch,
			checkFunc,
			12,
			http.StatusAccepted,
			false,
		},
		{
			"summary_case",
			&summaryBatch,
			checkFunc,
			10,
			http.StatusAccepted,
			false,
		},
		{
			"unmatchedBoundBucketIntHist_case",
			&unmatchedBoundBucketIntHistBatch,
			checkFunc,
			5,
			http.StatusAccepted,
			false,
		},
		{
			"unmatchedBoundBucketHist_case",
			&unmatchedBoundBucketHistBatch,
			checkFunc,
			5,
			http.StatusAccepted,
			false,
		},
		{
			"5xx_case",
			&unmatchedBoundBucketHistBatch,
			checkFunc,
			5,
			http.StatusServiceUnavailable,
			true,
		},
		{
			"emptyDoubleGauge_case",
			&emptyDoubleGaugeBatch,
			checkFunc,
			0,
			http.StatusAccepted,
			true,
		},
		{
			"emptyIntGauge_case",
			&emptyIntGaugeBatch,
			checkFunc,
			0,
			http.StatusAccepted,
			true,
		},
		{
			"emptyCumulativeDoubleSum_case",
			&emptyCumulativeDoubleSumBatch,
			checkFunc,
			0,
			http.StatusAccepted,
			true,
		},
		{
			"emptyCumulativeIntSum_case",
			&emptyCumulativeIntSumBatch,
			checkFunc,
			0,
			http.StatusAccepted,
			true,
		},
		{
			"emptyCumulativeHistogram_case",
			&emptyCumulativeHistogramBatch,
			checkFunc,
			0,
			http.StatusAccepted,
			true,
		},
		{
			"emptyCumulativeIntHistogram_case",
			&emptyCumulativeIntHistogramBatch,
			checkFunc,
			0,
			http.StatusAccepted,
			true,
		},
		{
			"emptyCumulativeSummary_case",
			&emptyCumulativeSummaryBatch,
			checkFunc,
			0,
			http.StatusAccepted,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.reqTestFunc != nil {
					tt.reqTestFunc(t, r, tt.expectedTimeSeries)
				}
				w.WriteHeader(tt.httpResponseCode)
			}))

			defer server.Close()

			serverURL, uErr := url.Parse(server.URL)
			assert.NoError(t, uErr)

			config := &Config{
				ExporterSettings: config.NewExporterSettings(config.NewID(typeStr)),
				Namespace:        "",
				HTTPClientSettings: confighttp.HTTPClientSettings{
					Endpoint: "http://some.url:9411/api/prom/push",
					// We almost read 0 bytes, so no need to tune ReadBufferSize.
					ReadBufferSize:  0,
					WriteBufferSize: 512 * 1024,
				},
			}
			assert.NotNil(t, config)
			// c, err := config.HTTPClientSettings.ToClient()
			// assert.Nil(t, err)
			c := http.DefaultClient
			buildInfo := component.BuildInfo{
				Description: "OpenTelemetry Collector",
				Version:     "1.0",
			}
			prwe, nErr := NewPrwExporter(config.Namespace, serverURL.String(), c, map[string]string{}, buildInfo)
			require.NoError(t, nErr)
			err := prwe.PushMetrics(context.Background(), *tt.md)
			if tt.returnErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func Test_validateAndSanitizeExternalLabels(t *testing.T) {
	tests := []struct {
		name           string
		inputLabels    map[string]string
		expectedLabels map[string]string
		returnError    bool
	}{
		{"success_case_no_labels",
			map[string]string{},
			map[string]string{},
			false,
		},
		{"success_case_with_labels",
			map[string]string{"key1": "val1"},
			map[string]string{"key1": "val1"},
			false,
		},
		{"success_case_2_with_labels",
			map[string]string{"__key1__": "val1"},
			map[string]string{"__key1__": "val1"},
			false,
		},
		{"success_case_with_sanitized_labels",
			map[string]string{"__key1.key__": "val1"},
			map[string]string{"__key1_key__": "val1"},
			false,
		},
		{"fail_case_empty_label",
			map[string]string{"": "val1"},
			map[string]string{},
			true,
		},
	}
	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newLabels, err := validateAndSanitizeExternalLabels(tt.inputLabels)
			if tt.returnError {
				assert.Error(t, err)
				return
			}
			assert.EqualValues(t, tt.expectedLabels, newLabels)
			assert.NoError(t, err)
		})
	}
}
