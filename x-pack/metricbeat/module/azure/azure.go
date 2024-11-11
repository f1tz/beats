// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package azure

import (
	"fmt"
	"sort"
	"time"

	"github.com/elastic/beats/v7/metricbeat/mb"
)

func init() {
	// Register the ModuleFactory function for the "azure" module.
	if err := mb.Registry.AddModule("azure", newModule); err != nil {
		panic(err)
	}
}

// newModule adds validation that hosts is non-empty, a requirement to use the
// azure module.
func newModule(base mb.BaseModule) (mb.Module, error) {
	return &base, nil
}

// MetricSet holds any configuration or state information. It must implement
// the mb.MetricSet interface. And this is best achieved by embedding
// mb.BaseMetricSet because it implements all of the required mb.MetricSet
// interface methods except for Fetch.
type MetricSet struct {
	mb.BaseMetricSet
	Client     *Client
	MapMetrics mapResourceMetrics
}

// NewMetricSet will instantiate a new azure metricset
func NewMetricSet(base mb.BaseMetricSet) (*MetricSet, error) {
	metricsetName := base.Name()
	var config Config
	err := base.Module().UnpackConfig(&config)
	if err != nil {
		return nil, fmt.Errorf("error unpack raw module config using UnpackConfig: %w", err)
	}

	//validate config based on metricset
	switch metricsetName {
	case nativeMetricset:
		// resources must be configured for the monitor metricset
		if len(config.Resources) == 0 {
			return nil, fmt.Errorf("no resource options defined: module azure - %s metricset", metricsetName)
		}
	default:
		// validate config resource options entered, no resource queries allowed for the compute_vm and compute_vm_scaleset metricsets
		for _, resource := range config.Resources {
			if resource.Query != "" {
				return nil, fmt.Errorf("error initializing the monitor client: module azure - %s metricset. No queries allowed, please select one of the allowed options", metricsetName)
			}
		}
		// check for lightweight resources if no groups or ids have been entered, if not a new resource is created to check the entire subscription
		var resources []ResourceConfig
		for _, resource := range config.Resources {
			if hasConfigOptions(resource.Group) || hasConfigOptions(resource.Id) {
				resources = append(resources, resource)
			}
		}
		// check if this is a light metricset or not and no resources have been configured
		if len(resources) == 0 && len(config.Resources) != 0 {
			resources = append(resources, ResourceConfig{
				Query:   fmt.Sprintf("resourceType eq '%s'", config.DefaultResourceType),
				Metrics: config.Resources[0].Metrics,
			})
		}
		config.Resources = resources
	}
	// instantiate monitor client
	monitorClient, err := NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("error initializing the monitor client: module azure - %s metricset: %w", metricsetName, err)
	}
	return &MetricSet{
		BaseMetricSet: base,
		Client:        monitorClient,
	}, nil
}

// Fetch methods implements the data gathering and data conversion to the right metricset
// It publishes the event which is then forwarded to the output. In case
// of an error set the Error field of mb.Event or simply call report.Error().
func (m *MetricSet) Fetch(report mb.ReporterV2) error {
	collectionStartTime := time.Now().UTC()

	// Set the reference time for the current fetch.
	//
	// The reference time is used to calculate time intervals
	// and compare with collection info in the metric
	// registry to decide whether to collect metrics or not,
	// depending on metric time grain (check `MetricRegistry`
	// for more information).
	//
	// We round the reference time to the nearest second to avoid
	// millisecond variations in the collection period causing
	// skipped collections.
	//
	// See "Round outer limits" and "Round inner limits" tests in
	// the metric_registry_test.go for more information.
	referenceTime := time.Now().UTC()

	// execute a function once

	// Refresh the list of cloud resources.
	resourceRefreshStartTime := time.Now().UTC()
	err := m.Client.RefreshResources()
	if err != nil {
		return fmt.Errorf("error getting resources: %w", err)
	}
	resourceRefreshEndTime := time.Now().UTC()

	//resourcesIDsWithDefinitions := make(chan string)

	definitionRefreshStartTime := time.Now().UTC()
	// sort the m.Client.Resources2 map by definition update time
	r := []*ResourceInfo{}
	for _, resourceInfo := range m.Client.Resources2 {
		r = append(r, resourceInfo)
	}

	sort.Slice(r, func(i, j int) bool {
		return r[i].definitionsUpdated.Before(r[j].definitionsUpdated)
	})

	//go func() {
	//defer close(resourcesIDsWithDefinitions)
	for _, resourceInfo := range r[:min(len(r), 50)] {
		//s.getResourceMetricsDefinitions(ctx, resourceID)
		go func(resourceInfo *ResourceInfo) {
			err := m.Client.RefreshResourceMetricsDefinitions(resourceInfo.resource.Id, m.MapMetrics)
			if err != nil {
				m.Client.Log.Errorf("error getting resource metrics definitions: %v", err)
			}
		}(resourceInfo)

		//resourcesIDsWithDefinitions <- resourceID
	}
	//}()
	definitionRefreshEndTime := time.Now().UTC()

	metricsDefinitions := m.Client.GetMetricsDefinitions()

	metricsValuesStartTime := time.Now().UTC()
	//metricValues := m.Client.GetMetricValues(referenceTime, metricsDefinitions, report)
	metricValues := m.Client.GetMetricValues2(referenceTime, metricsDefinitions, report)

	if err := mapToEvents(metricValues, m.Client, report); err != nil {
		m.Client.Log.Errorf("error mapping metrics to events: %v", err)
	}
	metricsValuesEndTime := time.Now().UTC()

	//var wg sync.WaitGroup
	//for resourceID := range resourcesIDsWithDefinitions {
	//	wg.Add(1)
	//	go func(resourceID string) {
	//		defer wg.Done()
	//		metricsDefinitions := m.Client.Resources2[resourceID].definitions
	//		m.Client.Log.Infow(
	//			"Getting resource metrics values",
	//			"resource_id", resourceID,
	//			"reference_time", referenceTime,
	//		)
	//
	//		metricValues := m.Client.GetMetricValues(referenceTime, metricsDefinitions, report)
	//
	//		// Turns metric values into events and sends them to Elasticsearch.
	//		if err := mapToEvents(metricValues, m.Client, report); err != nil {
	//			m.Client.Log.Errorf("error mapping metrics to events: %v", err)
	//		}
	//	}(resourceID)
	//}
	//wg.Wait()

	//
	//// Initialize cloud resources and monitor metrics
	//// information.
	////
	//// The client collects and stores:
	//// - existing cloud resource definitions (e.g. VMs, DBs, etc.)
	//// - metric definitions for the resources (e.g. CPU, memory, etc.)
	////
	//// The metricset periodically refreshes the information
	//// after `RefreshListInterval` (default 600s for
	//// most metricsets).
	//err = m.Client.InitResources(m.MapMetrics)
	//if err != nil {
	//	return err
	//}
	//
	//if len(m.Client.ResourceConfigurations.Metrics) == 0 {
	//	// error message is previously logged in the InitResources,
	//	// no error event should be created
	//	return nil
	//}
	//
	//// Group metric definitions by cloud resource ID.
	////
	//// We group the metric definitions by resource ID to fetch
	//// metric values for each cloud resource in one API call.
	//metricsByResourceId := groupMetricsDefinitionsByResourceId(m.Client.ResourceConfigurations.Metrics)
	//
	//for _, metricsDefinition := range metricsByResourceId {
	//	// Fetch metric values for each resource.
	//	metricValues := m.Client.GetMetricValues(referenceTime, metricsDefinition, report)
	//
	//	// Turns metric values into events and sends them to Elasticsearch.
	//	if err := mapToEvents(metricValues, m.Client, report); err != nil {
	//		return fmt.Errorf("error mapping metrics to events: %w", err)
	//	}
	//}

	collectionEndTime := time.Now().UTC()

	// Report the collection time.
	m.Client.Log.Infow(
		"Collection completed",
		"total_duration", collectionEndTime.Sub(collectionStartTime).Milliseconds(),
		"resources_duration", resourceRefreshEndTime.Sub(resourceRefreshStartTime).Milliseconds(),
		"definitions_duration", definitionRefreshEndTime.Sub(definitionRefreshStartTime).Milliseconds(),
		"definitions_available", len(metricsDefinitions),
		"values_duration", metricsValuesEndTime.Sub(metricsValuesStartTime).Milliseconds(),
		"resource_count", len(m.Client.Resources2),
		"throttling_ends_at", m.Client.EndOfMetricsDefinitionThrottling,
	)

	return nil
}

// hasConfigOptions func will check if any resource id or resource group options have been entered in the light metricsets
func hasConfigOptions(config []string) bool {
	if config == nil {
		return false
	}
	for _, group := range config {
		if group == "" {
			return false
		}
	}
	return true
}
