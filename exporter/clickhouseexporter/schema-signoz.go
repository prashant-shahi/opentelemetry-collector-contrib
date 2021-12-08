// Copyright  The OpenTelemetry Authors
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

package clickhouseexporter

import "encoding/json"

type Event struct {
	Name         string            `json:"name,omitempty"`
	TimeUnixNano uint64            `json:"timeUnixNano,omitempty"`
	AttributeMap map[string]string `json:"attributeMap,omitempty"`
}

type Span struct {
	TraceId            string        `json:"traceId,omitempty"`
	SpanId             string        `json:"spanId,omitempty"`
	ParentSpanId       string        `json:"parentSpanId,omitempty"`
	Name               string        `json:"name,omitempty"`
	DurationNano       uint64        `json:"durationNano,omitempty"`
	StartTimeUnixNano  uint64        `json:"startTimeUnixNano,omitempty"`
	ServiceName        string        `json:"serviceName,omitempty"`
	Kind               int32         `json:"kind,omitempty"`
	References         []OtelSpanRef `json:"references,omitempty"`
	Tags               []string      `json:"tags,omitempty"`
	TagsKeys           []string      `json:"tagsKeys,omitempty"`
	TagsValues         []string      `json:"tagsValues,omitempty"`
	StatusCode         int64         `json:"statusCode,omitempty"`
	ExternalHttpMethod string        `json:"externalHttpMethod,omitempty"`
	ExternalHttpUrl    string        `json:"externalHttpUrl,omitempty"`
	Component          string        `json:"component,omitempty"`
	DBSystem           string        `json:"dbSystem,omitempty"`
	DBName             string        `json:"dbName,omitempty"`
	DBOperation        string        `json:"dbOperation,omitempty"`
	PeerService        string        `json:"peerService,omitempty"`
	Events             []Event       `json:"event,omitempty"`
}

type OtelSpanRef struct {
	TraceId string `json:"traceId,omitempty"`
	SpanId  string `json:"spanId,omitempty"`
	RefType string `json:"refType,omitempty"`
}

func (span *Span) GetReferences() *string {
	value, err := json.Marshal(span.References)
	if err != nil {
		return nil
	}

	referencesString := string(value)
	return &referencesString
}
