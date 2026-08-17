package service

const (
	SchedulerOutboxEventAccountChanged       = "account_changed"
	SchedulerOutboxEventAccountGroupsChanged = "account_groups_changed"
	SchedulerOutboxEventAccountBulkChanged   = "account_bulk_changed"
	SchedulerOutboxEventAccountLastUsed      = "account_last_used"
	SchedulerOutboxEventGroupChanged         = "group_changed"
	SchedulerOutboxEventFullRebuild          = "full_rebuild"

	// SchedulerOutboxPayloadMetadataOnly keeps the durable event backward
	// compatible with older workers: new workers skip bucket rebuilds, while an
	// older worker safely treats the same account_changed event as a full refresh.
	SchedulerOutboxPayloadMetadataOnly = "metadata_only"
)
