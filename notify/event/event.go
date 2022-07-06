// Copyright 2019 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package event

import (
	"context"
	"io"
	"net/http"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	commoncfg "github.com/prometheus/common/config"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"

	"github.com/google/uuid"
)

// Notifier implements a Notifier for generic event.
type Notifier struct {
	conf    *config.EventConfig
	tmpl    *template.Template
	logger  log.Logger
	client  *http.Client
	retrier *notify.Retrier
}

// New returns a new Event.
func New(conf *config.EventConfig, t *template.Template, l log.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*conf.HTTPConfig, "event", httpOpts...)
	if err != nil {
		return nil, err
	}
	return &Notifier{
		conf:   conf,
		tmpl:   t,
		logger: l,
		client: client,
		// Event are assumed to respond with 2xx response codes on a successful
		// request and 5xx response codes are assumed to be recoverable.
		retrier: &notify.Retrier{
			CustomDetailsFunc: func(int, io.Reader) string {
				return conf.URL.String()
			},
		},
	}, nil
}

// Message defines the JSON object send to event endpoints.
type Message struct {
	*template.Data

	// The protocol version.
	Version         string `json:"version"`
	GroupKey        string `json:"groupKey"`
	TruncatedAlerts uint64 `json:"truncatedAlerts"`
}

func truncateAlerts(maxAlerts uint64, alerts []*types.Alert) ([]*types.Alert, uint64) {
	if maxAlerts != 0 && uint64(len(alerts)) > maxAlerts {
		return alerts[:maxAlerts], uint64(len(alerts)) - maxAlerts
	}

	return alerts, 0
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
	alerts, numTruncated := truncateAlerts(n.conf.MaxAlerts, alerts)
	data := notify.GetTemplateData(ctx, n.tmpl, alerts, n.logger)

	groupKey, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		level.Error(n.logger).Log("err", err)
	}

	msg := &Message{
		Version:         "4",
		Data:            data,
		GroupKey:        groupKey.String(),
		TruncatedAlerts: numTruncated,
	}

	eventUuid := uuid.New()
	event := cloudevents.NewEvent()
	event.SetID(eventUuid.String())
	event.SetSource(n.conf.Source)
	event.SetType("alert")
	event.SetData(cloudevents.ApplicationJSON, msg)

	c, err := cloudevents.NewClientHTTP()
	if err != nil {
		return false, err
	}

	ctx1 := cloudevents.ContextWithTarget(context.Background(), n.conf.URL.String())

	// Send that Event.
	result := c.Send(ctx1, event)
	if cloudevents.IsUndelivered(result) {
		return false, err
	}

	var httpResult *cehttp.Result
	cloudevents.ResultAs(result, &httpResult)

	return n.retrier.Check(httpResult.StatusCode, nil)
}
