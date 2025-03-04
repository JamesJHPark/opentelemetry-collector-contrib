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

package couchdbreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/couchdbreceiver"

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/receiver/scraperhelper"
)

func TestType(t *testing.T) {
	factory := NewFactory()
	ft := factory.Type()
	require.EqualValues(t, "couchdb", ft)
}

func TestValidConfig(t *testing.T) {
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.Username = "otel"
	cfg.Password = "otel"

	require.EqualValues(t, defaultEndpoint, cfg.Endpoint)
	require.NoError(t, cfg.Validate())
}

func TestCreateMetricsReceiver(t *testing.T) {
	factory := NewFactory()
	_, err := factory.CreateMetricsReceiver(
		context.Background(),
		component.ReceiverCreateSettings{},
		&Config{
			ScraperControllerSettings: scraperhelper.ScraperControllerSettings{
				ReceiverSettings:   config.NewReceiverSettings(config.NewComponentID("couchdb")),
				CollectionInterval: 10 * time.Second,
			},
			HTTPClientSettings: confighttp.HTTPClientSettings{
				Endpoint: "http://localhost:5984",
			},
			Username: "otel",
			Password: "otel",
		},
		consumertest.NewNop(),
	)
	require.NoError(t, err)
}
