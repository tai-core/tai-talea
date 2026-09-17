package obs

// Metric names required by development document §14. They are declared as
// constants so that dashboards and alerts cannot drift from the code.
const (
	MetricCapacityInstancesTotal = "capacity_instances_total"
	MetricServiceInstancesTotal  = "service_instances_total"
	MetricCapacityEventsTotal    = "capacity_events_total"
	MetricPartnerSyncSuccessTime = "partner_sync_success_timestamp"
	MetricPartnerSyncFailures    = "partner_sync_failures_total"
	MetricInstancePrepareSeconds = "instance_prepare_duration_seconds"
	MetricServiceStartSeconds    = "service_start_duration_seconds"
	MetricServiceDrainSeconds    = "service_drain_duration_seconds"
	MetricServiceDrainTimeout    = "service_drain_timeout_total"
	MetricLostInstancesTotal     = "lost_instances_total"
	MetricRouterRegFailures      = "router_registration_failures_total"
	MetricPDRatioCurrent         = "pd_ratio_current"
	MetricPDRatioTarget          = "pd_ratio_target"
	MetricPlannerChangesTotal    = "planner_changes_total"
)

// Label key sets, declared once to keep call sites consistent.
var (
	LabelsPartnerState = []string{"partner", "state"}
	LabelsRoleState    = []string{"role", "state"}
	LabelsEvent        = []string{"type", "source", "result"}
	LabelsPartner      = []string{"partner"}
	LabelsReason       = []string{"reason"}
	LabelsOutcome      = []string{"outcome"}
	LabelsPlannerRole  = []string{"role"}
)

// RecordInstanceState publishes the current container state distribution.
// It replaces the previous series for the same instance by recomputing gauges
// from the authoritative instance list.
func (r *Registry) RecordInstanceState(partner string, state string) {
	r.SetGauge(MetricCapacityInstancesTotal,
		"Container instances currently tracked by the control plane, by partner and instance state.",
		LabelsPartnerState, 0, "partner", partner, "state", state)
}

// AddInstanceStateCount publishes an absolute per-state count.
func (r *Registry) AddInstanceStateCount(partner string, state string, count int) {
	r.SetGauge(MetricCapacityInstancesTotal,
		"Container instances currently tracked by the control plane, by partner and instance state.",
		LabelsPartnerState, float64(count), "partner", partner, "state", state)
}

// AddServiceStateCount publishes an absolute per-role service count.
func (r *Registry) AddServiceStateCount(role string, state string, count int) {
	r.SetGauge(MetricServiceInstancesTotal,
		"Control-plane managed SGLang services, by PD role and service state.",
		LabelsRoleState, float64(count), "role", role, "state", state)
}

// RecordEvent increments the capacity event counter.
func (r *Registry) RecordEvent(eventType, source, result string) {
	r.IncCounter(MetricCapacityEventsTotal,
		"Capacity events observed by the control plane, by type, source and result.",
		LabelsEvent, "type", eventType, "source", source, "result", result)
}
