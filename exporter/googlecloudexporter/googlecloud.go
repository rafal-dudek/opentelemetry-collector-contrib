// Copyright 2019, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package googlecloudexporter contains the wrapper for OpenTelemetry-GoogleCloud
// exporter to be used in opentelemetry-collector.
package googlecloudexporter

import (
	"context"
	"contrib.go.opencensus.io/exporter/stackdriver"
	"fmt"
	cloudtrace "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	metricspb "github.com/census-instrumentation/opencensus-proto/gen-go/metrics/v1"
	resourcepb "github.com/census-instrumentation/opencensus-proto/gen-go/resource/v1"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/obsreport"
	"go.opentelemetry.io/collector/translator/conventions"
	"go.opentelemetry.io/collector/translator/internaldata"
	traceexport "go.opentelemetry.io/otel/sdk/export/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"strings"
)

// traceExporter is a wrapper struct of OT cloud trace exporter
type traceExporter struct {
	texporter *cloudtrace.Exporter
}

// metricsExporter is a wrapper struct of OC stackdriver exporter
type metricsExporter struct {
	mexporter          *stackdriver.Exporter
	labelsLimit        int
	LabelsToResources  []LabelsToResource
	loggerNoStacktrace *zap.Logger
	obsrep             *obsreport.Processor
}

func (te *traceExporter) Shutdown(ctx context.Context) error {
	return te.texporter.Shutdown(ctx)
}

func (me *metricsExporter) Shutdown(context.Context) error {
	me.mexporter.Flush()
	me.mexporter.StopMetricsExporter()
	return nil
}

func setVersionInUserAgent(cfg *Config, version string) {
	cfg.UserAgent = strings.ReplaceAll(cfg.UserAgent, "{{version}}", version)
}

func generateClientOptions(cfg *Config) ([]option.ClientOption, error) {
	var copts []option.ClientOption
	// option.WithUserAgent is used by the Trace exporter, but not the Metric exporter (see comment below)
	if cfg.UserAgent != "" {
		copts = append(copts, option.WithUserAgent(cfg.UserAgent))
	}
	if cfg.Endpoint != "" {
		if cfg.UseInsecure {
			// option.WithGRPCConn option takes precedent over all other supplied options so the
			// following user agent will be used by both exporters if we reach this branch
			var dialOpts []grpc.DialOption
			if cfg.UserAgent != "" {
				dialOpts = append(dialOpts, grpc.WithUserAgent(cfg.UserAgent))
			}
			conn, err := grpc.Dial(cfg.Endpoint, append(dialOpts, grpc.WithInsecure())...)
			if err != nil {
				return nil, fmt.Errorf("cannot configure grpc conn: %w", err)
			}
			copts = append(copts, option.WithGRPCConn(conn))
		} else {
			copts = append(copts, option.WithEndpoint(cfg.Endpoint))
		}
	}
	if cfg.GetClientOptions != nil {
		copts = append(copts, cfg.GetClientOptions()...)
	}
	if cfg.CredentialFileName != "" {
		copts = append(copts, option.WithCredentialsFile(cfg.CredentialFileName))
	}
	return copts, nil
}

func newGoogleCloudTraceExporter(cfg *Config, params component.ExporterCreateParams) (component.TracesExporter, error) {
	setVersionInUserAgent(cfg, params.ApplicationStartInfo.Version)

	topts := []cloudtrace.Option{
		cloudtrace.WithProjectID(cfg.ProjectID),
		cloudtrace.WithTimeout(cfg.Timeout),
	}

	copts, err := generateClientOptions(cfg)
	if err != nil {
		return nil, err
	}
	topts = append(topts, cloudtrace.WithTraceClientOptions(copts))

	exp, err := cloudtrace.NewExporter(topts...)
	if err != nil {
		return nil, fmt.Errorf("error creating GoogleCloud Trace exporter: %w", err)
	}

	tExp := &traceExporter{texporter: exp}

	return exporterhelper.NewTraceExporter(
		cfg,
		params.Logger,
		tExp.pushTraces,
		exporterhelper.WithShutdown(tExp.Shutdown),
		// Disable exporterhelper Timeout, since we are using a custom mechanism
		// within exporter itself
		exporterhelper.WithTimeout(exporterhelper.TimeoutSettings{Timeout: 0}),
		exporterhelper.WithQueue(cfg.QueueSettings),
		exporterhelper.WithRetry(cfg.RetrySettings))
}

func newGoogleCloudMetricsExporter(cfg *Config, params component.ExporterCreateParams) (component.MetricsExporter, error) {
	setVersionInUserAgent(cfg, params.ApplicationStartInfo.Version)

	// TODO:  For each ProjectID, create a different exporter
	// or at least a unique Google Cloud client per ProjectID.
	options := stackdriver.Options{
		// If the project ID is an empty string, it will be set by default based on
		// the project this is running on in GCP.
		ProjectID: cfg.ProjectID,

		MetricPrefix: cfg.MetricConfig.Prefix,

		// Set DefaultMonitoringLabels to an empty map to avoid getting the "opencensus_task" label
		DefaultMonitoringLabels: &stackdriver.Labels{},

		Timeout: cfg.Timeout,
	}

	// note options.UserAgent overrides the option.WithUserAgent client option in the Metric exporter
	if cfg.UserAgent != "" {
		options.UserAgent = cfg.UserAgent
	}

	copts, err := generateClientOptions(cfg)
	if err != nil {
		return nil, err
	}
	options.TraceClientOptions = copts
	options.MonitoringClientOptions = copts

	if cfg.MetricConfig.SkipCreateMetricDescriptor {
		options.SkipCMD = true
	}
	if len(cfg.ResourceMappings) > 0 {
		rm := resourceMapper{
			mappings: cfg.ResourceMappings,
		}
		options.MapResource = rm.mapResource
	}

	obsrep := obsreport.NewProcessor(obsreport.ProcessorSettings{
		Level:         configtelemetry.GetMetricsLevelFlagValue(),
		ProcessorName: cfg.Name(),
	})

	sde, serr := stackdriver.NewExporter(options)
	if serr != nil {
		return nil, fmt.Errorf("cannot configure Google Cloud metric exporter: %w", serr)
	}
	mExp := &metricsExporter{mexporter: sde, labelsLimit: cfg.LabelsLimit, LabelsToResources: cfg.LabelsToResources,
		loggerNoStacktrace: params.Logger.WithOptions(zap.AddStacktrace(zapcore.PanicLevel)), obsrep: obsrep}

	return exporterhelper.NewMetricsExporter(
		cfg,
		params.Logger,
		mExp.pushMetrics,
		exporterhelper.WithResourceToTelemetryConversion(cfg.ResourceToTelemetrySettings),
		exporterhelper.WithShutdown(mExp.Shutdown),
		// Disable exporterhelper Timeout, since we are using a custom mechanism
		// within exporter itself
		exporterhelper.WithTimeout(exporterhelper.TimeoutSettings{Timeout: 0}),
		exporterhelper.WithQueue(cfg.QueueSettings),
		exporterhelper.WithRetry(cfg.RetrySettings))
}

// pushMetrics calls StackdriverExporter.PushMetricsProto on each element of the given metrics
func (me *metricsExporter) pushMetrics(ctx context.Context, m pdata.Metrics) error {
	// Some metrics from Hostmetric receiver has Datapoints with empty StartTimestamp = 0
	// This causes that later on StartTimestamp is assigned as Timestamp and such datapoints are rejected by google monitoring api
	fixEmptyStartTime(m)

	// PushMetricsProto doesn't bundle subsequent calls, so we need to
	// combine the data here to avoid generating too many RPC calls.
	mds := exportAdditionalLabels(internaldata.MetricsToOC(m))
	count := 0
	for i := 0; i < len(mds); i++ {
		if len(me.LabelsToResources) > 0 {
			me.mapLabelsToResource(&mds[i])
		}
		if me.labelsLimit > 0 { // drop metrics with labels count greater then labelsLimit
			for _, metric := range mds[i].Metrics {
				if len(metric.GetMetricDescriptor().GetLabelKeys()) <= me.labelsLimit {
					count++
				} else {
					me.loggerNoStacktrace.Warn("Dropping metric: too many labels",
						zap.String("metric", metric.GetMetricDescriptor().GetName()),
						zap.Int("labels", len(metric.GetMetricDescriptor().GetLabelKeys())),
						zap.Int("limit", me.labelsLimit))
				}
			}
		} else {
			count += len(mds[i].Metrics)
		}
	}
	if count == 0 {
		me.loggerNoStacktrace.Warn("Dropping sending whole batch: no metrics, because all dropped, reason: too many labels",
			zap.Int("limit", me.labelsLimit))
		return nil
	}

	metrics := make([]*metricspb.Metric, 0, count)
	for _, md := range mds {
		if md.Resource == nil && me.labelsLimit == 0 {
			metrics = append(metrics, md.Metrics...)
			continue
		}
		for _, metric := range md.Metrics {
			if me.labelsLimit > 0 && len(metric.GetMetricDescriptor().GetLabelKeys()) > me.labelsLimit {
				// drop metrics with labels count greater then labelsLimit
				me.obsrep.MetricsRefused(ctx, len(metric.Timeseries))
				continue
			}
			if metric.Resource == nil && md.Resource != nil {
				metric.Resource = md.Resource
			}
			metrics = append(metrics, metric)
		}
	}
	points := numPoints(metrics)
	// The two nil args here are: node (which is ignored) and resource
	// (which we just moved to individual metrics).
	dropped, err := me.mexporter.PushMetricsProto(ctx, nil, nil, metrics)
	recordPointCount(ctx, points-dropped, dropped, err)
	return err
}

func (me *metricsExporter) mapLabelsToResource(md *internaldata.MetricsData) {
	metrics := make([]*metricspb.Metric, 0, len(md.Metrics))
	for _, metric := range md.Metrics {

		if metric.Resource == nil && md.Resource != nil {
			metric.Resource = md.Resource
		}
		found := false
		for _, ltr := range me.LabelsToResources {
			if me.labelsLimit == 0 || me.labelsLimit >= len(metric.GetMetricDescriptor().GetLabelKeys())-len(ltr.LabelToResources) {
				for _, labelKey := range metric.MetricDescriptor.LabelKeys {
					if labelKey.Key == ltr.RequiredLabel {
						labelKeys := append([]*metricspb.LabelKey(nil), metric.MetricDescriptor.LabelKeys...)

						indices := make([]int, 0, len(ltr.LabelToResources))
						for _, mapping := range ltr.LabelToResources {
							for i := 0; i < len(labelKeys); i++ {
								if labelKeys[i].Key == mapping.SourceLabel {
									indices = append(indices, i)
									labelKeys[i] = labelKeys[len(labelKeys)-1]
									labelKeys = labelKeys[:len(labelKeys)-1]
									break
								}
							}
						}
						if len(indices) < len(ltr.LabelToResources) {
							me.loggerNoStacktrace.Debug("Mapping failed: ",
								zap.String("metric", metric.GetMetricDescriptor().String()))
							break
						}
						found = true
						metric.MetricDescriptor.LabelKeys = labelKeys
						for _, ts := range metric.Timeseries {
							resourceLabels := make(map[string]string)
							if metric.Resource != nil {
								for k, v := range metric.Resource.Labels {
									resourceLabels[k] = v
								}
							}

							for iTarget, iSource := range indices {
								resourceLabels[ltr.LabelToResources[iTarget].TargetResourceLabel] = ts.LabelValues[iSource].Value
								ts.LabelValues[iSource] = ts.LabelValues[len(ts.LabelValues)-1]
								ts.LabelValues = ts.LabelValues[:len(ts.LabelValues)-1]
							}

							//TODO: Optimize, do not create Metric for each ts if the same Resource labels
							metrics = append(metrics, &metricspb.Metric{
								Timeseries:       []*metricspb.TimeSeries{ts},
								MetricDescriptor: metric.MetricDescriptor,
								Resource: &resourcepb.Resource{
									Type:   ltr.TargetType,
									Labels: resourceLabels,
								},
							})
						}
						break
					}
				}
			} else {
				me.loggerNoStacktrace.Debug("Skipping Mapping: ",
					zap.String("metric", metric.GetMetricDescriptor().String()))
			}
			if found {
				break
			}
		}
		if !found {
			metrics = append(metrics, metric)
		}
	}
	md.Metrics = metrics
}

func exportAdditionalLabels(mds []internaldata.MetricsData) []internaldata.MetricsData {
	for _, md := range mds {
		if md.Resource == nil ||
			md.Resource.Labels == nil ||
			md.Node == nil ||
			md.Node.Identifier == nil ||
			len(md.Node.Identifier.HostName) == 0 {
			continue
		}
		// MetricsToOC removes `host.name` label and writes it to node indentifier, here we reintroduce it.
		md.Resource.Labels[conventions.AttributeHostName] = md.Node.Identifier.HostName
	}
	return mds
}

// pushTraces calls texporter.ExportSpan for each span in the given traces
func (te *traceExporter) pushTraces(ctx context.Context, td pdata.Traces) error {
	var errs []error
	resourceSpans := td.ResourceSpans()
	spans := make([]*traceexport.SpanSnapshot, 0, td.SpanCount())
	for i := 0; i < resourceSpans.Len(); i++ {
		sd := pdataResourceSpansToOTSpanData(resourceSpans.At(i))
		spans = append(spans, sd...)
	}

	err := te.texporter.ExportSpans(ctx, spans)
	if err != nil {
		errs = append(errs, err)
	}
	return consumererror.Combine(errs)
}

func numPoints(metrics []*metricspb.Metric) int {
	numPoints := 0
	for _, metric := range metrics {
		tss := metric.GetTimeseries()
		for _, ts := range tss {
			numPoints += len(ts.GetPoints())
		}
	}
	return numPoints
}