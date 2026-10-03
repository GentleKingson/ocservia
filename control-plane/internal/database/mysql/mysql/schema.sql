CREATE TABLE `backend_schema_snapshot` (
  `singleton` tinyint NOT NULL,
  `artifact_checksum` varbinary(64) NOT NULL,
  `state` varbinary(16) NOT NULL,
  `repair_count` int NOT NULL DEFAULT '0',
  `started_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `verified_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`singleton`),
  CONSTRAINT `backend_schema_snapshot_chk_1` CHECK ((`singleton` = 1)),
  CONSTRAINT `backend_schema_snapshot_chk_2` CHECK ((`state` in (_utf8mb4'running',_utf8mb4'verified')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `backend_schema_snapshot_steps` (
  `ordinal` int NOT NULL,
  `name` varbinary(64) NOT NULL,
  `checksum` varbinary(64) NOT NULL,
  `state` varbinary(16) NOT NULL,
  `started_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `verified_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`ordinal`),
  UNIQUE KEY `name` (`name`),
  CONSTRAINT `backend_schema_snapshot_steps_chk_1` CHECK ((`state` in (_utf8mb4'running',_utf8mb4'verified')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `backend_migration_steps` (
  `ordinal` int NOT NULL,
  `name` varbinary(64) NOT NULL,
  `checksum` char(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `state` varchar(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `started_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `verified_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`ordinal`),
  UNIQUE KEY `name` (`name`),
  CONSTRAINT `backend_migration_steps_chk_1` CHECK ((`state` in (_utf8mb4'running',_utf8mb4'verified')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `backend_migrations` (
  `singleton` tinyint NOT NULL,
  `engine` varbinary(16) NOT NULL,
  `manifest_checksum` char(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `version` int NOT NULL DEFAULT '0',
  `dirty` tinyint(1) NOT NULL DEFAULT '1',
  `controller_schema` int NOT NULL DEFAULT '0',
  `minimum_controller_schema` int NOT NULL DEFAULT '0',
  `repair_count` int NOT NULL DEFAULT '0',
  `updated_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`singleton`),
  CONSTRAINT `backend_migrations_chk_1` CHECK ((`singleton` = 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `backend_schema_revisions` (
  `version` int NOT NULL,
  `parent_checksum` varbinary(64) NOT NULL,
  `manifest_checksum` varbinary(64) NOT NULL,
  `state` varbinary(16) NOT NULL,
  `repair_count` int NOT NULL DEFAULT '0',
  `started_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `verified_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`version`),
  CONSTRAINT `backend_schema_revisions_chk_1` CHECK ((`version` >= 2)),
  CONSTRAINT `backend_schema_revisions_chk_2` CHECK ((`state` in (_utf8mb4'running',_utf8mb4'verified')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `business_locks` (
  `lock_key` varbinary(128) NOT NULL,
  PRIMARY KEY (`lock_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `connection_owner_fencing` (
  `node_id` varbinary(512) NOT NULL,
  `owner_instance_id` varbinary(16) NOT NULL,
  `owner_incarnation` bigint NOT NULL,
  `connection_id` longblob NOT NULL,
  `owner_epoch` bigint NOT NULL,
  `lease_until` bigint NOT NULL,
  `updated_at` bigint NOT NULL DEFAULT (timestampdiff(MICROSECOND,_utf8mb4'2000-01-01',now(6))),
  PRIMARY KEY (`node_id`),
  CONSTRAINT `connection_owner_fencing_chk_1` CHECK ((length(`owner_instance_id`) = 16)),
  CONSTRAINT `connection_owner_fencing_connection_owner_fencing_c_0c3e39836699` CHECK ((length(`connection_id`) = 16)),
  CONSTRAINT `connection_owner_fencing_connection_owner_fencing_node_id_check` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `connection_owner_fencing_connection_owner_fencing_o_bcb93078cf4c` CHECK ((`owner_epoch` >= 1)),
  CONSTRAINT `connection_owner_fencing_connection_owner_fencing_o_f97fdc9d8e9d` CHECK ((`owner_incarnation` >= 0)),
  CONSTRAINT `connection_owner_fencing_lease_until_range` CHECK (((`lease_until` is null) or (`lease_until` in (-(9223372036854775808),9223372036854775807)) or ((`lease_until` >= -(211813488000000000)) and (`lease_until` < 9223371331200000000)))),
  CONSTRAINT `connection_owner_fencing_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `controller_schema_compatibility` (
  `singleton` tinyint(1) NOT NULL DEFAULT '1',
  `current_schema` bigint NOT NULL,
  `minimum_compatible_controller_schema` bigint NOT NULL,
  PRIMARY KEY (`singleton`),
  CONSTRAINT `controller_schema_compatibility_chk_1` CHECK ((`singleton` in (0,1))),
  CONSTRAINT `controller_schema_compatibility_controller_schema_c_057f91eb81b0` CHECK ((`current_schema` > 0)),
  CONSTRAINT `controller_schema_compatibility_controller_schema_c_4367681c918c` CHECK ((`minimum_compatible_controller_schema` > 0)),
  CONSTRAINT `controller_schema_compatibility_controller_schema_c_981ffc9e96ac` CHECK ((`minimum_compatible_controller_schema` <= `current_schema`)),
  CONSTRAINT `controller_schema_compatibility_controller_schema_c_d8ed1bb45f00` CHECK ((`singleton` = 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `exact_key_guards` (
  `key_name` varbinary(64) NOT NULL,
  PRIMARY KEY (`key_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `identities` (
  `id` varbinary(16) NOT NULL,
  `issuer` longtext NOT NULL,
  `subject` longtext NOT NULL,
  `email` longtext,
  `display_name` longtext,
  `disabled_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  CONSTRAINT `identities_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `identities_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `identities_disabled_at_range` CHECK (((`disabled_at` is null) or (`disabled_at` in (-(9223372036854775808),9223372036854775807)) or ((`disabled_at` >= -(211813488000000000)) and (`disabled_at` < 9223371331200000000)))),
  CONSTRAINT `identities_text_no_nul` CHECK (((locate(0x00,cast(`issuer` as char charset binary)) = 0) and (locate(0x00,cast(`subject` as char charset binary)) = 0) and (locate(0x00,cast(`email` as char charset binary)) = 0) and (locate(0x00,cast(`display_name` as char charset binary)) = 0))),
  CONSTRAINT `identities_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `local_auth_attempts` (
  `username` varchar(191) NOT NULL,
  `failures` int NOT NULL DEFAULT '0',
  `lease_id` varbinary(16) DEFAULT NULL,
  `window_until` bigint NOT NULL,
  `blocked_until` bigint NOT NULL DEFAULT '-9223372036854775808',
  `lease_until` bigint NOT NULL DEFAULT '-9223372036854775808',
  `expires_at` bigint NOT NULL,
  PRIMARY KEY (`username`),
  KEY `local_auth_attempts_expiry` (`expires_at`),
  CONSTRAINT `local_auth_attempts_blocked_until_range` CHECK (((`blocked_until` is null) or (`blocked_until` in (-(9223372036854775808),9223372036854775807)) or ((`blocked_until` >= -(211813488000000000)) and (`blocked_until` < 9223371331200000000)))),
  CONSTRAINT `local_auth_attempts_chk_1` CHECK ((length(`lease_id`) = 16)),
  CONSTRAINT `local_auth_attempts_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -(211813488000000000)) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `local_auth_attempts_lease_until_range` CHECK (((`lease_until` is null) or (`lease_until` in (-(9223372036854775808),9223372036854775807)) or ((`lease_until` >= -(211813488000000000)) and (`lease_until` < 9223371331200000000)))),
  CONSTRAINT `local_auth_attempts_local_auth_attempts_failures_check` CHECK (((`failures` >= 0) and (`failures` <= 14))),
  CONSTRAINT `local_auth_attempts_local_auth_attempts_username_check` CHECK (((length(`username`) >= 1) and (length(`username`) <= 128) and regexp_like(`username`,_utf8mb4'(?-i)\\A[a-z0-9][a-z0-9._-]*\\z'))),
  CONSTRAINT `local_auth_attempts_text_no_nul` CHECK ((locate(0x00,cast(`username` as char charset binary)) = 0)),
  CONSTRAINT `local_auth_attempts_window_until_range` CHECK (((`window_until` is null) or (`window_until` in (-(9223372036854775808),9223372036854775807)) or ((`window_until` >= -(211813488000000000)) and (`window_until` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `local_credentials` (
  `identity_id` varbinary(16) NOT NULL,
  `username` varchar(191) NOT NULL,
  `password_hash` longtext NOT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `password_changed_at` bigint NOT NULL,
  PRIMARY KEY (`identity_id`),
  UNIQUE KEY `local_credentials_username_key` (`username`),
  CONSTRAINT `local_credentials_identity_id_fkey` FOREIGN KEY (`identity_id`) REFERENCES `identities` (`id`) ON DELETE CASCADE,
  CONSTRAINT `local_credentials_chk_1` CHECK ((length(`identity_id`) = 16)),
  CONSTRAINT `local_credentials_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `local_credentials_local_credentials_password_hash_check` CHECK (((length(`password_hash`) >= 1) and (length(`password_hash`) <= 512))),
  CONSTRAINT `local_credentials_local_credentials_username_check` CHECK (((length(`username`) >= 1) and (length(`username`) <= 128) and regexp_like(`username`,_utf8mb4'(?-i)\\A[a-z0-9][a-z0-9._-]*\\z'))),
  CONSTRAINT `local_credentials_password_changed_at_range` CHECK (((`password_changed_at` is null) or (`password_changed_at` in (-(9223372036854775808),9223372036854775807)) or ((`password_changed_at` >= -(211813488000000000)) and (`password_changed_at` < 9223371331200000000)))),
  CONSTRAINT `local_credentials_text_no_nul` CHECK (((locate(0x00,cast(`username` as char charset binary)) = 0) and (locate(0x00,cast(`password_hash` as char charset binary)) = 0))),
  CONSTRAINT `local_credentials_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_agent_upgrade_results` (
  `operation_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `state` longtext NOT NULL,
  `target_version` longtext NOT NULL,
  `detail` longtext NOT NULL DEFAULT (_utf8mb4''),
  `privileged_result_proof` longblob NOT NULL,
  `completed_at` bigint NOT NULL,
  `reported_at` bigint NOT NULL,
  PRIMARY KEY (`operation_id`),
  KEY `node_agent_upgrade_results_node_idx` (`node_id`),
  CONSTRAINT `node_agent_upgrade_results_chk_1` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `node_agent_upgrade_results_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_agent_upgrade_results_completed_at_range` CHECK (((`completed_at` is null) or (`completed_at` in (-(9223372036854775808),9223372036854775807)) or ((`completed_at` >= -(211813488000000000)) and (`completed_at` < 9223371331200000000)))),
  CONSTRAINT `node_agent_upgrade_results_node_agent_upgrade_resul_3e58bfa07965` CHECK (((length(`privileged_result_proof`) >= 1) and (length(`privileged_result_proof`) <= 65536))),
  CONSTRAINT `node_agent_upgrade_results_node_agent_upgrade_resul_a329fbaf47c7` CHECK ((`state` in (_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'rolled_back'))),
  CONSTRAINT `node_agent_upgrade_results_node_agent_upgrade_resul_c609f04cc978` CHECK ((char_length(`detail`) <= 160)),
  CONSTRAINT `node_agent_upgrade_results_reported_at_range` CHECK (((`reported_at` is null) or (`reported_at` in (-(9223372036854775808),9223372036854775807)) or ((`reported_at` >= -(211813488000000000)) and (`reported_at` < 9223371331200000000)))),
  CONSTRAINT `node_agent_upgrade_results_text_no_nul` CHECK (((locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`target_version` as char charset binary)) = 0) and (locate(0x00,cast(`detail` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `roles` (
  `name` varchar(191) NOT NULL,
  PRIMARY KEY (`name`),
  CONSTRAINT `roles_roles_name_check` CHECK ((`name` in (_utf8mb4'Viewer',_utf8mb4'Operator',_utf8mb4'UserManager',_utf8mb4'ConfigManager',_utf8mb4'Auditor',_utf8mb4'SecurityAdmin',_utf8mb4'PlatformAdmin'))),
  CONSTRAINT `roles_text_no_nul` CHECK ((locate(0x00,cast(`name` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `scheduler_leadership` (
  `id` int NOT NULL,
  `instance_id` varbinary(16) NOT NULL,
  `incarnation` bigint NOT NULL,
  `epoch` bigint NOT NULL,
  `lease_until` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  CONSTRAINT `scheduler_leadership_chk_1` CHECK ((length(`instance_id`) = 16)),
  CONSTRAINT `scheduler_leadership_lease_until_range` CHECK (((`lease_until` is null) or (`lease_until` in (-(9223372036854775808),9223372036854775807)) or ((`lease_until` >= -(211813488000000000)) and (`lease_until` < 9223371331200000000)))),
  CONSTRAINT `scheduler_leadership_scheduler_leadership_epoch_check` CHECK ((`epoch` >= 0)),
  CONSTRAINT `scheduler_leadership_scheduler_leadership_id_check` CHECK ((`id` = 1)),
  CONSTRAINT `scheduler_leadership_scheduler_leadership_incarnation_check` CHECK ((`incarnation` >= 0)),
  CONSTRAINT `scheduler_leadership_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `scheduler_leases` (
  `lease_name` varchar(191) NOT NULL,
  `owner_id` varbinary(16) NOT NULL,
  `lease_until` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`lease_name`),
  CONSTRAINT `scheduler_leases_chk_1` CHECK ((length(`owner_id`) = 16)),
  CONSTRAINT `scheduler_leases_lease_until_range` CHECK (((`lease_until` is null) or (`lease_until` in (-(9223372036854775808),9223372036854775807)) or ((`lease_until` >= -(211813488000000000)) and (`lease_until` < 9223371331200000000)))),
  CONSTRAINT `scheduler_leases_scheduler_leases_lease_name_check` CHECK (((char_length(`lease_name`) >= 1) and (char_length(`lease_name`) <= 128))),
  CONSTRAINT `scheduler_leases_text_no_nul` CHECK ((locate(0x00,cast(`lease_name` as char charset binary)) = 0)),
  CONSTRAINT `scheduler_leases_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `telemetry_legacy_migration` (
  `singleton` tinyint NOT NULL,
  `state` varbinary(16) NOT NULL,
  `completed_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`singleton`),
  CONSTRAINT `telemetry_legacy_migration_chk_1` CHECK ((`singleton` = 1)),
  CONSTRAINT `telemetry_legacy_migration_chk_2` CHECK ((`state` in (_utf8mb4'pending',_utf8mb4'running',_utf8mb4'complete'))),
  CONSTRAINT `telemetry_legacy_migration_chk_3` CHECK (((`state` = _utf8mb4'complete') = (`completed_at` is not null)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `telemetry_maintenance_page` (
  `slot` smallint NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `metric` longblob NOT NULL,
  `bucket_at` bigint NOT NULL,
  `sample_count` bigint NOT NULL,
  `min_value` double NOT NULL,
  `max_value` double NOT NULL,
  `avg_value` double NOT NULL,
  PRIMARY KEY (`slot`),
  CONSTRAINT `telemetry_maintenance_page_chk_1` CHECK ((`slot` between 1 and 80))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `telemetry_maintenance_progress` (
  `singleton` tinyint NOT NULL,
  `phase` tinyint NOT NULL DEFAULT '0',
  `cutoff` bigint NOT NULL DEFAULT '0',
  `candidate` varbinary(64) DEFAULT NULL,
  `cursor_at` bigint DEFAULT NULL,
  `cursor_node` varbinary(16) DEFAULT NULL,
  `cursor_metric` longblob,
  PRIMARY KEY (`singleton`),
  CONSTRAINT `telemetry_maintenance_progress_chk_1` CHECK ((`singleton` = 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `telemetry_rollups_1h` (
  `node_id` varbinary(16) NOT NULL,
  `metric` longtext NOT NULL,
  `sample_count` bigint NOT NULL,
  `min_value` double NOT NULL,
  `max_value` double NOT NULL,
  `avg_value` double NOT NULL,
  `bucket_at` bigint NOT NULL,
  `exact_row_id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `short_metric` varbinary(255) GENERATED ALWAYS AS ((case when (length(`metric`) <= 255) then cast(`metric` as char charset binary) else NULL end)) STORED,
  PRIMARY KEY (`exact_row_id`),
  UNIQUE KEY `exact_row_id_key` (`exact_row_id`),
  UNIQUE KEY `telemetry_short_key` (`node_id`,`bucket_at`,`short_metric`),
  KEY `exact_node_lookup` (`node_id`,`bucket_at`),
  KEY `telemetry_1h_retention_idx` (`bucket_at`,`exact_row_id`),
  CONSTRAINT `telemetry_rollups_1h_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `telemetry_rollups_1h_telemetry_rollups_5m_sample_count_check` CHECK ((`sample_count` > 0)),
  CONSTRAINT `telemetry_rollups_1h_text_no_nul` CHECK ((locate(0x00,cast(`metric` as char charset binary)) = 0)),
  CONSTRAINT `telemetry_rollups_1h_time_range` CHECK (((`bucket_at` in (-(9223372036854775808),9223372036854775807)) or ((`bucket_at` >= -211813488000000000) and (`bucket_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `telemetry_sample_shards` (
  `table_name` varbinary(64) NOT NULL,
  `start_at` bigint NOT NULL,
  `end_at` bigint NOT NULL,
  `state` varbinary(8) NOT NULL,
  PRIMARY KEY (`table_name`),
  UNIQUE KEY `start_at` (`start_at`),
  KEY `telemetry_retirement_lookup` (`state`,`end_at`,`start_at`),
  CONSTRAINT `telemetry_sample_shards_chk_1` CHECK ((`state` in (_utf8mb4'planned',_utf8mb4'active',_utf8mb4'retired',_utf8mb4'dropped'))),
  CONSTRAINT `telemetry_sample_shards_chk_2` CHECK ((`end_at` > `start_at`))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `time_migration_decisions` (
  `table_name` varbinary(64) NOT NULL,
  `column_name` varbinary(64) NOT NULL,
  `row_key` varbinary(128) NOT NULL,
  `source_value` datetime(6) NOT NULL,
  `decision` varbinary(24) NOT NULL,
  PRIMARY KEY (`table_name`,`column_name`,`row_key`),
  CONSTRAINT `time_migration_decisions_chk_1` CHECK ((`decision` in (_utf8mb4'finite',_utf8mb4'negative_infinity')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `transport_event_cursor` (
  `singleton` tinyint(1) NOT NULL DEFAULT '1',
  `event_id` varbinary(16) NOT NULL,
  `valid` tinyint(1) NOT NULL DEFAULT '1',
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`singleton`),
  CONSTRAINT `transport_event_cursor_chk_1` CHECK ((`singleton` in (0,1))),
  CONSTRAINT `transport_event_cursor_chk_2` CHECK ((length(`event_id`) = 16)),
  CONSTRAINT `transport_event_cursor_chk_3` CHECK ((`valid` in (0,1))),
  CONSTRAINT `transport_event_cursor_transport_event_cursor_singleton_check` CHECK ((`singleton` = 1)),
  CONSTRAINT `transport_event_cursor_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `transport_event_quarantine` (
  `event_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `event_type` int NOT NULL,
  `payload_sha256` longblob NOT NULL,
  `reason_code` longtext NOT NULL,
  `reason_detail` longtext NOT NULL,
  `observed_at` bigint NOT NULL,
  PRIMARY KEY (`event_id`),
  KEY `transport_event_quarantine_node_time_idx` (`node_id`,`observed_at` DESC),
  CONSTRAINT `transport_event_quarantine_chk_1` CHECK ((length(`event_id`) = 16)),
  CONSTRAINT `transport_event_quarantine_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `transport_event_quarantine_observed_at_range` CHECK (((`observed_at` is null) or (`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -(211813488000000000)) and (`observed_at` < 9223371331200000000)))),
  CONSTRAINT `transport_event_quarantine_text_no_nul` CHECK (((locate(0x00,cast(`reason_code` as char charset binary)) = 0) and (locate(0x00,cast(`reason_detail` as char charset binary)) = 0))),
  CONSTRAINT `transport_event_quarantine_transport_event_quaranti_3d26b180e849` CHECK (regexp_like(`reason_code`,_utf8mb4'(?-i)\\A[a-z][a-z0-9_]{0,63}\\z')),
  CONSTRAINT `transport_event_quarantine_transport_event_quaranti_85dc61464f5a` CHECK ((length(`payload_sha256`) = 32)),
  CONSTRAINT `transport_event_quarantine_transport_event_quaranti_e73de9135d30` CHECK (((length(`reason_detail`) >= 1) and (length(`reason_detail`) <= 256)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `upstream_sync_records` (
  `id` varbinary(16) NOT NULL,
  `repository` longtext NOT NULL,
  `old_ref` longtext NOT NULL,
  `old_commit` varchar(191) NOT NULL,
  `new_ref` longtext NOT NULL,
  `new_commit` varchar(191) NOT NULL,
  `rollback_ref` longtext NOT NULL,
  `synced_at` bigint NOT NULL,
  `classification` longblob NOT NULL,
  PRIMARY KEY (`id`),
  CONSTRAINT `upstream_sync_records_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `upstream_sync_records_synced_at_range` CHECK (((`synced_at` is null) or (`synced_at` in (-(9223372036854775808),9223372036854775807)) or ((`synced_at` >= -211813488000000000) and (`synced_at` < 9223371331200000000)))),
  CONSTRAINT `upstream_sync_records_text_no_nul` CHECK (((locate(0x00,cast(`repository` as char charset binary)) = 0) and (locate(0x00,cast(`old_ref` as char charset binary)) = 0) and (locate(0x00,cast(`old_commit` as char charset binary)) = 0) and (locate(0x00,cast(`new_ref` as char charset binary)) = 0) and (locate(0x00,cast(`new_commit` as char charset binary)) = 0) and (locate(0x00,cast(`rollback_ref` as char charset binary)) = 0))),
  CONSTRAINT `upstream_sync_records_upstream_sync_records_new_commit_check` CHECK (regexp_like(`new_commit`,_utf8mb4'(?-i)\\A[0-9a-f]{40}\\z')),
  CONSTRAINT `upstream_sync_records_upstream_sync_records_old_commit_check` CHECK (regexp_like(`old_commit`,_utf8mb4'(?-i)\\A[0-9a-f]{40}\\z'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `workspaces` (
  `id` varbinary(16) NOT NULL,
  `name` longtext NOT NULL,
  `slug` longtext NOT NULL,
  `version` bigint NOT NULL DEFAULT '1',
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `archived_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  CONSTRAINT `workspaces_archived_at_range` CHECK (((`archived_at` is null) or (`archived_at` in (-(9223372036854775808),9223372036854775807)) or ((`archived_at` >= -(211813488000000000)) and (`archived_at` < 9223371331200000000)))),
  CONSTRAINT `workspaces_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `workspaces_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `workspaces_text_no_nul` CHECK (((locate(0x00,cast(`name` as char charset binary)) = 0) and (locate(0x00,cast(`slug` as char charset binary)) = 0))),
  CONSTRAINT `workspaces_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000)))),
  CONSTRAINT `workspaces_workspaces_version_check` CHECK ((`version` > 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `approval_requests` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `requester_id` varbinary(16) NOT NULL,
  `action` varchar(191) NOT NULL,
  `resource_type` varchar(191) NOT NULL,
  `resource_id` varbinary(16) NOT NULL,
  `reason` longtext NOT NULL,
  `status` varchar(191) NOT NULL,
  `approver_id` varbinary(16) DEFAULT NULL,
  `approval_reason` longtext,
  `request_hash` longblob,
  `expires_at` bigint NOT NULL,
  `created_at` bigint NOT NULL,
  `consumed_at` bigint DEFAULT NULL,
  `approved_at` bigint DEFAULT NULL,
  `authority_snapshot_at` bigint NOT NULL DEFAULT (timestampdiff(MICROSECOND,_utf8mb4'2000-01-01',utc_timestamp(6))),
  `request_summary` longblob,
  PRIMARY KEY (`id`),
  KEY `approval_requests_approver_id_fkey` (`approver_id`),
  KEY `approval_requests_requester_id_fkey` (`requester_id`),
  KEY `approval_requests_scope_idx` (`workspace_id`,`resource_type`,`resource_id`,`action`,`status`,`expires_at`),
  CONSTRAINT `approval_requests_approver_id_fkey` FOREIGN KEY (`approver_id`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `approval_requests_requester_id_fkey` FOREIGN KEY (`requester_id`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `approval_requests_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `approval_requests_approval_request_content_pair` CHECK (((`request_hash` is null) = (`request_summary` is null))),
  CONSTRAINT `approval_requests_approval_requests_action_check` CHECK (((char_length(`action`) >= 1) and (char_length(`action`) <= 128))),
  CONSTRAINT `approval_requests_approval_requests_check` CHECK ((not((`requester_id` <=> `approver_id`)))),
  CONSTRAINT `approval_requests_approval_requests_check1` CHECK (((`status` in (_utf8mb4'approved',_utf8mb4'consumed')) = (`approver_id` is not null))),
  CONSTRAINT `approval_requests_approval_requests_check2` CHECK (((`status` = _utf8mb4'consumed') = (`consumed_at` is not null))),
  CONSTRAINT `approval_requests_approval_requests_check3` CHECK ((`expires_at` > `created_at`)),
  CONSTRAINT `approval_requests_approval_requests_reason_check` CHECK (((char_length(`reason`) >= 1) and (char_length(`reason`) <= 512))),
  CONSTRAINT `approval_requests_approval_requests_request_hash_check` CHECK (((`request_hash` is null) or (length(`request_hash`) = 32))),
  CONSTRAINT `approval_requests_approval_requests_resource_type_check` CHECK (((char_length(`resource_type`) >= 1) and (char_length(`resource_type`) <= 64))),
  CONSTRAINT `approval_requests_approval_requests_status_check` CHECK ((`status` in (_utf8mb4'pending',_utf8mb4'approved',_utf8mb4'rejected',_utf8mb4'expired',_utf8mb4'consumed'))),
  CONSTRAINT `approval_requests_approved_at_range` CHECK (((`approved_at` is null) or (`approved_at` in (-(9223372036854775808),9223372036854775807)) or ((`approved_at` >= -211813488000000000) and (`approved_at` < 9223371331200000000)))),
  CONSTRAINT `approval_requests_authority_snapshot_at_range` CHECK (((`authority_snapshot_at` is null) or (`authority_snapshot_at` in (-(9223372036854775808),9223372036854775807)) or ((`authority_snapshot_at` >= -211813488000000000) and (`authority_snapshot_at` < 9223371331200000000)))),
  CONSTRAINT `approval_requests_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `approval_requests_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `approval_requests_chk_3` CHECK ((length(`requester_id`) = 16)),
  CONSTRAINT `approval_requests_chk_4` CHECK ((length(`resource_id`) = 16)),
  CONSTRAINT `approval_requests_chk_5` CHECK ((length(`approver_id`) = 16)),
  CONSTRAINT `approval_requests_consumed_at_range` CHECK (((`consumed_at` is null) or (`consumed_at` in (-(9223372036854775808),9223372036854775807)) or ((`consumed_at` >= -211813488000000000) and (`consumed_at` < 9223371331200000000)))),
  CONSTRAINT `approval_requests_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -211813488000000000) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `approval_requests_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -211813488000000000) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `approval_requests_text_no_nul` CHECK (((locate(0x00,cast(`action` as char charset binary)) = 0) and (locate(0x00,cast(`resource_type` as char charset binary)) = 0) and (locate(0x00,cast(`reason` as char charset binary)) = 0) and (locate(0x00,cast(`status` as char charset binary)) = 0) and (locate(0x00,cast(`approval_reason` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `auth_sessions` (
  `id` varbinary(16) NOT NULL,
  `identity_id` varbinary(16) NOT NULL,
  `break_glass` tinyint(1) NOT NULL DEFAULT '0',
  `expires_at` bigint NOT NULL,
  `revoked_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  KEY `auth_sessions_identity_id_fkey` (`identity_id`),
  KEY `auth_sessions_expiry_idx` (`expires_at`),
  CONSTRAINT `auth_sessions_identity_id_fkey` FOREIGN KEY (`identity_id`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `auth_sessions_auth_sessions_check` CHECK ((`expires_at` > `created_at`)),
  CONSTRAINT `auth_sessions_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `auth_sessions_chk_2` CHECK ((length(`identity_id`) = 16)),
  CONSTRAINT `auth_sessions_chk_3` CHECK ((`break_glass` in (0,1))),
  CONSTRAINT `auth_sessions_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `auth_sessions_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -(211813488000000000)) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `auth_sessions_revoked_at_range` CHECK (((`revoked_at` is null) or (`revoked_at` in (-(9223372036854775808),9223372036854775807)) or ((`revoked_at` >= -(211813488000000000)) and (`revoked_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `backend_schema_revision_steps` (
  `version` int NOT NULL,
  `ordinal` int NOT NULL,
  `name` varbinary(64) NOT NULL,
  `checksum` varbinary(64) NOT NULL,
  `state` varbinary(16) NOT NULL,
  `started_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `verified_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`version`,`ordinal`),
  UNIQUE KEY `version` (`version`,`name`),
  CONSTRAINT `backend_schema_revision_steps_ibfk_1` FOREIGN KEY (`version`) REFERENCES `backend_schema_revisions` (`version`) ON DELETE RESTRICT,
  CONSTRAINT `backend_schema_revision_steps_chk_1` CHECK ((`state` in (_utf8mb4'running',_utf8mb4'verified')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `batch_operations` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `state` longtext NOT NULL,
  `actor_identity_id` varbinary(16) DEFAULT NULL,
  `actor_session_id` varbinary(16) DEFAULT NULL,
  `approval_id` varbinary(16) DEFAULT NULL,
  `actor_id` longtext NOT NULL,
  `reason` longtext NOT NULL,
  `request_id` longtext NOT NULL,
  `traceparent` longtext NOT NULL,
  `idempotency_key` varchar(191) NOT NULL,
  `request_hash` longblob NOT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `batch_operations_workspace_id_idempotency_key_key` (`workspace_id`,`idempotency_key`),
  KEY `batch_operations_actor_identity_id_fkey` (`actor_identity_id`),
  KEY `batch_operations_actor_session_id_fkey` (`actor_session_id`),
  KEY `batch_operations_approval_id_fkey` (`approval_id`),
  KEY `batch_operations_active_idx` (`updated_at`,`id`),
  CONSTRAINT `batch_operations_actor_identity_id_fkey` FOREIGN KEY (`actor_identity_id`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `batch_operations_actor_session_id_fkey` FOREIGN KEY (`actor_session_id`) REFERENCES `auth_sessions` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `batch_operations_approval_id_fkey` FOREIGN KEY (`approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `batch_operations_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `batch_operations_batch_operations_actor_id_check` CHECK (((char_length(`actor_id`) >= 1) and (char_length(`actor_id`) <= 256))),
  CONSTRAINT `batch_operations_batch_operations_idempotency_key_check` CHECK (((char_length(`idempotency_key`) >= 1) and (char_length(`idempotency_key`) <= 128))),
  CONSTRAINT `batch_operations_batch_operations_reason_check` CHECK (((char_length(`reason`) >= 1) and (char_length(`reason`) <= 512))),
  CONSTRAINT `batch_operations_batch_operations_request_hash_check` CHECK ((length(`request_hash`) = 32)),
  CONSTRAINT `batch_operations_batch_operations_request_id_check` CHECK (((char_length(`request_id`) >= 1) and (char_length(`request_id`) <= 128))),
  CONSTRAINT `batch_operations_batch_operations_state_check` CHECK ((`state` in (_utf8mb4'queued',_utf8mb4'running',_utf8mb4'succeeded',_utf8mb4'partial_failed',_utf8mb4'failed'))),
  CONSTRAINT `batch_operations_batch_operations_traceparent_check` CHECK (regexp_like(`traceparent`,_utf8mb4'(?-i)\\A00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}\\z')),
  CONSTRAINT `batch_operations_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `batch_operations_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `batch_operations_chk_3` CHECK ((length(`actor_identity_id`) = 16)),
  CONSTRAINT `batch_operations_chk_4` CHECK ((length(`actor_session_id`) = 16)),
  CONSTRAINT `batch_operations_chk_5` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `batch_operations_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `batch_operations_text_no_nul` CHECK (((locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`actor_id` as char charset binary)) = 0) and (locate(0x00,cast(`reason` as char charset binary)) = 0) and (locate(0x00,cast(`request_id` as char charset binary)) = 0) and (locate(0x00,cast(`traceparent` as char charset binary)) = 0) and (locate(0x00,cast(`idempotency_key` as char charset binary)) = 0))),
  CONSTRAINT `batch_operations_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `break_glass_uses` (
  `credential_fingerprint` varbinary(512) NOT NULL,
  `identity_id` varbinary(16) NOT NULL,
  `source_session_id` varbinary(16) NOT NULL,
  `rotation_required` tinyint(1) NOT NULL DEFAULT '1',
  `used_at` bigint NOT NULL,
  PRIMARY KEY (`credential_fingerprint`),
  KEY `break_glass_uses_identity_id_fkey` (`identity_id`),
  CONSTRAINT `break_glass_uses_identity_id_fkey` FOREIGN KEY (`identity_id`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `break_glass_uses_break_glass_uses_credential_fingerprint_check` CHECK ((length(`credential_fingerprint`) = 32)),
  CONSTRAINT `break_glass_uses_chk_1` CHECK ((length(`identity_id`) = 16)),
  CONSTRAINT `break_glass_uses_chk_2` CHECK ((length(`source_session_id`) = 16)),
  CONSTRAINT `break_glass_uses_chk_3` CHECK ((`rotation_required` in (0,1))),
  CONSTRAINT `break_glass_uses_used_at_range` CHECK (((`used_at` is null) or (`used_at` in (-(9223372036854775808),9223372036854775807)) or ((`used_at` >= -(211813488000000000)) and (`used_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `exact_identities` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_identities_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `identities` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `exact_telemetry_rollups_1h` (
  `owner_id` bigint unsigned NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  KEY `telemetry_key_lookup` (`key_value`(255)),
  CONSTRAINT `exact_telemetry_rollups_1h_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `telemetry_rollups_1h` (`exact_row_id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `exact_upstream_sync_records` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_upstream_sync_records_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `upstream_sync_records` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `exact_workspaces` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_workspaces_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `local_auth_bootstrap` (
  `singleton` tinyint(1) NOT NULL DEFAULT '1',
  `identity_id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `completion_pending` tinyint(1) NOT NULL DEFAULT '0',
  `approver_identity_id` varbinary(16) DEFAULT NULL,
  `completed_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`singleton`),
  KEY `local_auth_bootstrap_approver_identity_id_fkey` (`approver_identity_id`),
  KEY `local_auth_bootstrap_identity_id_fkey` (`identity_id`),
  KEY `local_auth_bootstrap_workspace_id_fkey` (`workspace_id`),
  CONSTRAINT `local_auth_bootstrap_approver_identity_id_fkey` FOREIGN KEY (`approver_identity_id`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `local_auth_bootstrap_identity_id_fkey` FOREIGN KEY (`identity_id`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `local_auth_bootstrap_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `local_auth_bootstrap_chk_1` CHECK ((`singleton` in (0,1))),
  CONSTRAINT `local_auth_bootstrap_chk_2` CHECK ((length(`identity_id`) = 16)),
  CONSTRAINT `local_auth_bootstrap_chk_3` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `local_auth_bootstrap_chk_4` CHECK ((`completion_pending` in (0,1))),
  CONSTRAINT `local_auth_bootstrap_chk_5` CHECK ((length(`approver_identity_id`) = 16)),
  CONSTRAINT `local_auth_bootstrap_completed_at_range` CHECK (((`completed_at` is null) or (`completed_at` in (-(9223372036854775808),9223372036854775807)) or ((`completed_at` >= -211813488000000000) and (`completed_at` < 9223371331200000000)))),
  CONSTRAINT `local_auth_bootstrap_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `local_auth_bootstrap_local_auth_bootstrap_singleton_check` CHECK ((`singleton` = 1)),
  CONSTRAINT `local_auth_bootstrap_local_initialization_state` CHECK ((((0 <> `completion_pending`) and (`completed_at` is null) and (`approver_identity_id` is null)) or ((0 = `completion_pending`) and (`completed_at` is not null))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `nodes` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `name` longtext NOT NULL,
  `status` longtext NOT NULL,
  `version` bigint NOT NULL DEFAULT '1',
  `policy` longtext,
  `authorization_revision` bigint NOT NULL DEFAULT '1',
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `labels` longblob NOT NULL DEFAULT (_utf8mb4'{}'),
  PRIMARY KEY (`id`),
  UNIQUE KEY `nodes_workspace_id_id_key` (`workspace_id`,`id`),
  CONSTRAINT `nodes_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `nodes_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `nodes_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `nodes_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -211813488000000000) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `nodes_nodes_authorization_revision_check` CHECK ((`authorization_revision` > 0)),
  CONSTRAINT `nodes_nodes_status_check` CHECK ((`status` in (_utf8mb4'pending',_utf8mb4'active',_utf8mb4'revoked',_utf8mb4'offline'))),
  CONSTRAINT `nodes_nodes_version_check` CHECK ((`version` > 0)),
  CONSTRAINT `nodes_text_no_nul` CHECK (((locate(0x00,cast(`name` as char charset binary)) = 0) and (locate(0x00,cast(`status` as char charset binary)) = 0) and (locate(0x00,cast(`policy` as char charset binary)) = 0))),
  CONSTRAINT `nodes_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -211813488000000000) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `observed_groups` (
  `node_id` varbinary(16) NOT NULL,
  `group_name` varchar(191) NOT NULL,
  `revision` bigint NOT NULL,
  `fingerprint` longblob NOT NULL,
  `observed_at` bigint NOT NULL,
  `members` longblob NOT NULL,
  PRIMARY KEY (`node_id`,`group_name`),
  CONSTRAINT `observed_groups_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `observed_groups_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `observed_groups_observed_groups_fingerprint_check` CHECK ((length(`fingerprint`) = 32)),
  CONSTRAINT `observed_groups_observed_groups_group_name_check` CHECK (regexp_like(`group_name`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z')),
  CONSTRAINT `observed_groups_observed_groups_revision_check` CHECK ((`revision` >= 0)),
  CONSTRAINT `observed_groups_text_no_nul` CHECK ((locate(0x00,cast(`group_name` as char charset binary)) = 0)),
  CONSTRAINT `observed_groups_time_range` CHECK (((`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -211813488000000000) and (`observed_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `observed_user_usage` (
  `node_id` varbinary(16) NOT NULL,
  `username` varchar(191) NOT NULL,
  `period` varchar(191) NOT NULL,
  `rx_bytes` bigint NOT NULL DEFAULT '0',
  `tx_bytes` bigint NOT NULL DEFAULT '0',
  `period_start` bigint NOT NULL,
  `observed_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`,`username`,`period`,`period_start`),
  CONSTRAINT `observed_user_usage_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `observed_user_usage_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `observed_user_usage_observed_at_range` CHECK (((`observed_at` is null) or (`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -(211813488000000000)) and (`observed_at` < 9223371331200000000)))),
  CONSTRAINT `observed_user_usage_observed_user_usage_period_check` CHECK ((`period` in (_utf8mb4'monthly',_utf8mb4'lifetime'))),
  CONSTRAINT `observed_user_usage_observed_user_usage_rx_bytes_check` CHECK ((`rx_bytes` >= 0)),
  CONSTRAINT `observed_user_usage_observed_user_usage_tx_bytes_check` CHECK ((`tx_bytes` >= 0)),
  CONSTRAINT `observed_user_usage_observed_user_usage_username_check` CHECK (regexp_like(`username`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z')),
  CONSTRAINT `observed_user_usage_period_start_range` CHECK (((`period_start` is null) or (`period_start` in (-(9223372036854775808),9223372036854775807)) or ((`period_start` >= -(211813488000000000)) and (`period_start` < 9223371331200000000)))),
  CONSTRAINT `observed_user_usage_text_no_nul` CHECK (((locate(0x00,cast(`username` as char charset binary)) = 0) and (locate(0x00,cast(`period` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `observed_users` (
  `node_id` varbinary(16) NOT NULL,
  `username` varchar(191) NOT NULL,
  `enabled` tinyint(1) NOT NULL,
  `revision` bigint NOT NULL,
  `fingerprint` longblob NOT NULL,
  `observed_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`,`username`),
  CONSTRAINT `observed_users_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `observed_users_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `observed_users_chk_2` CHECK ((`enabled` in (0,1))),
  CONSTRAINT `observed_users_observed_at_range` CHECK (((`observed_at` is null) or (`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -(211813488000000000)) and (`observed_at` < 9223371331200000000)))),
  CONSTRAINT `observed_users_observed_users_fingerprint_check` CHECK ((length(`fingerprint`) = 32)),
  CONSTRAINT `observed_users_observed_users_revision_check` CHECK ((`revision` >= 0)),
  CONSTRAINT `observed_users_observed_users_username_check` CHECK (regexp_like(`username`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z')),
  CONSTRAINT `observed_users_text_no_nul` CHECK ((locate(0x00,cast(`username` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `operations` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) DEFAULT NULL,
  `command_id` varbinary(16) DEFAULT NULL,
  `state` varchar(191) NOT NULL,
  `version` bigint NOT NULL DEFAULT '1',
  `request_id` longtext NOT NULL,
  `trace_id` longtext,
  `idempotency_key` longtext,
  `request_hash` longblob,
  `partial_460753916246` tinyint GENERATED ALWAYS AS ((case when (`idempotency_key` is not null) then 1 else NULL end)) STORED,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `expires_at` bigint DEFAULT NULL,
  `completed_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `operations_queued_backlog_idx` (`workspace_id`,`node_id`,`id`),
  KEY `operations_active_command_limit_idx` (`state`,`id`),
  KEY `operations_workspace_created_idx` (`workspace_id`,`created_at` DESC,`id` DESC),
  CONSTRAINT `operations_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `operations_workspace_id_node_id_fkey` FOREIGN KEY (`workspace_id`, `node_id`) REFERENCES `nodes` (`workspace_id`, `id`) ON DELETE RESTRICT,
  CONSTRAINT `operations_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `operations_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `operations_chk_3` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `operations_chk_4` CHECK ((length(`command_id`) = 16)),
  CONSTRAINT `operations_completed_at_range` CHECK (((`completed_at` is null) or (`completed_at` in (-(9223372036854775808),9223372036854775807)) or ((`completed_at` >= -(211813488000000000)) and (`completed_at` < 9223371331200000000)))),
  CONSTRAINT `operations_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `operations_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -(211813488000000000)) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `operations_operations_idempotency_pair` CHECK (((`idempotency_key` is null) = (`request_hash` is null))),
  CONSTRAINT `operations_operations_request_hash_size` CHECK (((`request_hash` is null) or (length(`request_hash`) = 32))),
  CONSTRAINT `operations_operations_state_check` CHECK ((`state` in (_utf8mb4'draft',_utf8mb4'queued',_utf8mb4'dispatched',_utf8mb4'accepted',_utf8mb4'running',_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'unknown',_utf8mb4'expired',_utf8mb4'rolled_back',_utf8mb4'offline_pending',_utf8mb4'drifted',_utf8mb4'superseded'))),
  CONSTRAINT `operations_operations_version_check` CHECK ((`version` > 0)),
  CONSTRAINT `operations_text_no_nul` CHECK (((locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`request_id` as char charset binary)) = 0) and (locate(0x00,cast(`trace_id` as char charset binary)) = 0) and (locate(0x00,cast(`idempotency_key` as char charset binary)) = 0))),
  CONSTRAINT `operations_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `privd_attestation_enrollment_credentials` (
  `id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `secret_sha256` varbinary(512) NOT NULL,
  `controller_nonce` longblob NOT NULL,
  `credential_context_sha256` longblob NOT NULL,
  `created_by_identity_id` varbinary(16) NOT NULL,
  `created_by_session_id` varbinary(16) NOT NULL,
  `expires_at` bigint NOT NULL,
  `consumed_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL DEFAULT (timestampdiff(MICROSECOND,_utf8mb4'2000-01-01 00:00:00',now(6))),
  PRIMARY KEY (`id`),
  UNIQUE KEY `privd_attestation_enrollment_credentials_secret_sha256_key` (`secret_sha256`),
  KEY `privd_attestation_enrollment_creden_created_by_identity_id_fkey` (`created_by_identity_id`),
  KEY `privd_attestation_enrollment_credent_created_by_session_id_fkey` (`created_by_session_id`),
  KEY `privd_attestation_credentials_node_active_idx` (`node_id`,`expires_at`),
  CONSTRAINT `privd_attestation_enrollment_creden_created_by_identity_id_fkey` FOREIGN KEY (`created_by_identity_id`) REFERENCES `identities` (`id`),
  CONSTRAINT `privd_attestation_enrollment_credent_created_by_session_id_fkey` FOREIGN KEY (`created_by_session_id`) REFERENCES `auth_sessions` (`id`),
  CONSTRAINT `privd_attestation_enrollment_credentials_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `privd_attestation_enrollment_credentials_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `privd_attestation_enrollment_credentials_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `privd_attestation_enrollment_credentials_chk_3` CHECK ((length(`created_by_identity_id`) = 16)),
  CONSTRAINT `privd_attestation_enrollment_credentials_chk_4` CHECK ((length(`created_by_session_id`) = 16)),
  CONSTRAINT `privd_attestation_enrollment_credentials_consumed_at_range` CHECK (((`consumed_at` is null) or (`consumed_at` in (-(9223372036854775808),9223372036854775807)) or ((`consumed_at` >= -(211813488000000000)) and (`consumed_at` < 9223371331200000000)))),
  CONSTRAINT `privd_attestation_enrollment_credentials_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `privd_attestation_enrollment_credentials_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -(211813488000000000)) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `privd_attestation_enrollment_credentials_privd_atte_44d13e165fc4` CHECK ((length(`credential_context_sha256`) = 32)),
  CONSTRAINT `privd_attestation_enrollment_credentials_privd_atte_4f7a46e8df72` CHECK (((`consumed_at` is null) or (`consumed_at` >= `created_at`))),
  CONSTRAINT `privd_attestation_enrollment_credentials_privd_atte_96cebf9d41c4` CHECK ((`expires_at` > `created_at`)),
  CONSTRAINT `privd_attestation_enrollment_credentials_privd_atte_af10fe8dcdd6` CHECK ((length(`secret_sha256`) = 32)),
  CONSTRAINT `privd_attestation_enrollment_credentials_privd_atte_b395bfbcfa51` CHECK ((length(`controller_nonce`) = 32)),
  CONSTRAINT `privd_attestation_enrollment_credentials_privd_atte_fe973e6d2f9e` CHECK (((ord(substr(`id`,7,1)) >> 4) = 7))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `role_bindings` (
  `id` varbinary(16) NOT NULL,
  `identity_id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `role_name` varchar(191) NOT NULL,
  `resource_type` varchar(191) NOT NULL DEFAULT 'workspace',
  `resource_id` varbinary(16) DEFAULT NULL,
  `created_by` varbinary(16) DEFAULT NULL,
  `approval_id` varbinary(16) DEFAULT NULL,
  `resource_id_unique` varbinary(17) GENERATED ALWAYS AS (if((`resource_id` is null),0x00,concat(0x01,`resource_id`))) STORED,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `role_bindings_identity_id_workspace_id_role_name_resource_t_key` (`identity_id`,`workspace_id`,`role_name`,`resource_type`,`resource_id_unique`),
  KEY `role_bindings_approval_id_fkey` (`approval_id`),
  KEY `role_bindings_created_by_fkey` (`created_by`),
  KEY `role_bindings_role_name_fkey` (`role_name`),
  KEY `role_bindings_workspace_id_fkey` (`workspace_id`),
  KEY `role_bindings_authorization_idx` (`identity_id`,`workspace_id`,`resource_type`,`resource_id`,`role_name`),
  CONSTRAINT `role_bindings_approval_id_fkey` FOREIGN KEY (`approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `role_bindings_created_by_fkey` FOREIGN KEY (`created_by`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `role_bindings_identity_id_fkey` FOREIGN KEY (`identity_id`) REFERENCES `identities` (`id`) ON DELETE CASCADE,
  CONSTRAINT `role_bindings_role_name_fkey` FOREIGN KEY (`role_name`) REFERENCES `roles` (`name`) ON DELETE RESTRICT,
  CONSTRAINT `role_bindings_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE,
  CONSTRAINT `role_bindings_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `role_bindings_chk_2` CHECK ((length(`identity_id`) = 16)),
  CONSTRAINT `role_bindings_chk_3` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `role_bindings_chk_4` CHECK ((length(`resource_id`) = 16)),
  CONSTRAINT `role_bindings_chk_5` CHECK ((length(`created_by`) = 16)),
  CONSTRAINT `role_bindings_chk_6` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `role_bindings_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `role_bindings_role_bindings_check` CHECK (((`resource_type` = _utf8mb4'workspace') = (`resource_id` is null))),
  CONSTRAINT `role_bindings_role_bindings_resource_type_check` CHECK ((`resource_type` in (_utf8mb4'workspace',_utf8mb4'node',_utf8mb4'resource',_utf8mb4'secret_ref',_utf8mb4'certificate',_utf8mb4'config_plan',_utf8mb4'batch_operation',_utf8mb4'role_binding'))),
  CONSTRAINT `role_bindings_text_no_nul` CHECK (((locate(0x00,cast(`role_name` as char charset binary)) = 0) and (locate(0x00,cast(`resource_type` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `secret_provider_refs` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `provider` varchar(191) NOT NULL,
  `key_path` varchar(513) NOT NULL,
  `version` longtext NOT NULL,
  `state` longtext NOT NULL,
  `rotated_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `secret_provider_refs_workspace_id_provider_key_path_key` (`workspace_id`,`provider`,`key_path`),
  CONSTRAINT `secret_provider_refs_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `secret_provider_refs_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `secret_provider_refs_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `secret_provider_refs_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `secret_provider_refs_rotated_at_range` CHECK (((`rotated_at` is null) or (`rotated_at` in (-(9223372036854775808),9223372036854775807)) or ((`rotated_at` >= -(211813488000000000)) and (`rotated_at` < 9223371331200000000)))),
  CONSTRAINT `secret_provider_refs_secret_provider_refs_key_path_check` CHECK (((char_length(`key_path`) >= 1) and (char_length(`key_path`) <= 512) and (not(regexp_like(`key_path`,_utf8mb4'(?-i)(\\A|/)\\.\\.(/|\\z)'))))),
  CONSTRAINT `secret_provider_refs_secret_provider_refs_provider_check` CHECK (regexp_like(`provider`,_utf8mb4'(?-i)\\A[A-Za-z0-9._-]{1,64}\\z')),
  CONSTRAINT `secret_provider_refs_secret_provider_refs_state_check` CHECK ((`state` in (_utf8mb4'active',_utf8mb4'rotating',_utf8mb4'disabled',_utf8mb4'unavailable'))),
  CONSTRAINT `secret_provider_refs_secret_provider_refs_version_check` CHECK (((char_length(`version`) >= 1) and (char_length(`version`) <= 128))),
  CONSTRAINT `secret_provider_refs_text_no_nul` CHECK (((locate(0x00,cast(`provider` as char charset binary)) = 0) and (locate(0x00,cast(`key_path` as char charset binary)) = 0) and (locate(0x00,cast(`version` as char charset binary)) = 0) and (locate(0x00,cast(`state` as char charset binary)) = 0))),
  CONSTRAINT `secret_provider_refs_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `security_alerts` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) DEFAULT NULL,
  `severity` longtext NOT NULL,
  `kind` longtext NOT NULL,
  `source_session_id` varbinary(16) DEFAULT NULL,
  `node_id` varbinary(16) DEFAULT NULL,
  `resource_type` longtext,
  `resource_id` varbinary(16) DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `acknowledged_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `security_alerts_node_id_fkey` (`node_id`),
  KEY `security_alerts_workspace_id_fkey` (`workspace_id`),
  CONSTRAINT `security_alerts_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `security_alerts_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `security_alerts_acknowledged_at_range` CHECK (((`acknowledged_at` is null) or (`acknowledged_at` in (-(9223372036854775808),9223372036854775807)) or ((`acknowledged_at` >= -(211813488000000000)) and (`acknowledged_at` < 9223371331200000000)))),
  CONSTRAINT `security_alerts_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `security_alerts_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `security_alerts_chk_3` CHECK ((length(`source_session_id`) = 16)),
  CONSTRAINT `security_alerts_chk_4` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `security_alerts_chk_5` CHECK ((length(`resource_id`) = 16)),
  CONSTRAINT `security_alerts_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `security_alerts_security_alerts_kind_check` CHECK (((char_length(`kind`) >= 1) and (char_length(`kind`) <= 128))),
  CONSTRAINT `security_alerts_security_alerts_resource_type_check` CHECK (((`resource_type` is null) or ((char_length(`resource_type`) >= 1) and (char_length(`resource_type`) <= 64)))),
  CONSTRAINT `security_alerts_security_alerts_severity_check` CHECK ((`severity` in (_utf8mb4'high',_utf8mb4'critical'))),
  CONSTRAINT `security_alerts_text_no_nul` CHECK (((locate(0x00,cast(`severity` as char charset binary)) = 0) and (locate(0x00,cast(`kind` as char charset binary)) = 0) and (locate(0x00,cast(`resource_type` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `telemetry_ingest_batches` (
  `batch_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `sequence` bigint NOT NULL,
  `kind` longtext NOT NULL,
  `payload_bytes` int NOT NULL,
  `observed_at` bigint NOT NULL,
  `received_at` bigint NOT NULL DEFAULT (timestampdiff(MICROSECOND,_utf8mb4'2000-01-01 00:00:00',now(6))),
  PRIMARY KEY (`batch_id`),
  KEY `telemetry_ingest_batches_node_id_fkey` (`node_id`),
  CONSTRAINT `telemetry_ingest_batches_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `telemetry_ingest_batches_chk_1` CHECK ((length(`batch_id`) = 16)),
  CONSTRAINT `telemetry_ingest_batches_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `telemetry_ingest_batches_observed_at_range` CHECK (((`observed_at` is null) or (`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -(211813488000000000)) and (`observed_at` < 9223371331200000000)))),
  CONSTRAINT `telemetry_ingest_batches_received_at_range` CHECK (((`received_at` is null) or (`received_at` in (-(9223372036854775808),9223372036854775807)) or ((`received_at` >= -(211813488000000000)) and (`received_at` < 9223371331200000000)))),
  CONSTRAINT `telemetry_ingest_batches_telemetry_ingest_batches_kind_check` CHECK ((`kind` in (_utf8mb4'security',_utf8mb4'current_health',_utf8mb4'aggregate',_utf8mb4'raw_history'))),
  CONSTRAINT `telemetry_ingest_batches_telemetry_ingest_batches_p_9437eacc68bc` CHECK (((`payload_bytes` >= 0) and (`payload_bytes` <= 524288))),
  CONSTRAINT `telemetry_ingest_batches_telemetry_ingest_batches_sequence_check` CHECK ((`sequence` >= 0)),
  CONSTRAINT `telemetry_ingest_batches_text_no_nul` CHECK ((locate(0x00,cast(`kind` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `telemetry_rollups_5m` (
  `node_id` varbinary(16) NOT NULL,
  `metric` longtext NOT NULL,
  `sample_count` bigint NOT NULL,
  `min_value` double NOT NULL,
  `max_value` double NOT NULL,
  `avg_value` double NOT NULL,
  `bucket_at` bigint NOT NULL,
  `exact_row_id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `short_metric` varbinary(255) GENERATED ALWAYS AS ((case when (length(`metric`) <= 255) then cast(`metric` as char charset binary) else NULL end)) STORED,
  PRIMARY KEY (`exact_row_id`),
  UNIQUE KEY `exact_row_id_key` (`exact_row_id`),
  UNIQUE KEY `telemetry_short_key` (`node_id`,`bucket_at`,`short_metric`),
  KEY `exact_node_lookup` (`node_id`,`bucket_at`),
  KEY `telemetry_5m_retention_idx` (`bucket_at`,`exact_row_id`),
  CONSTRAINT `telemetry_rollups_5m_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `telemetry_rollups_5m_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `telemetry_rollups_5m_telemetry_rollups_5m_sample_count_check` CHECK ((`sample_count` > 0)),
  CONSTRAINT `telemetry_rollups_5m_text_no_nul` CHECK ((locate(0x00,cast(`metric` as char charset binary)) = 0)),
  CONSTRAINT `telemetry_rollups_5m_time_range` CHECK (((`bucket_at` in (-(9223372036854775808),9223372036854775807)) or ((`bucket_at` >= -211813488000000000) and (`bucket_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `telemetry_samples` (
  `node_id` varbinary(16) NOT NULL,
  `batch_id` varbinary(16) NOT NULL,
  `metric` varchar(191) NOT NULL,
  `value` double NOT NULL,
  `sampled_at` bigint NOT NULL,
  PRIMARY KEY (`sampled_at`,`node_id`,`batch_id`,`metric`),
  KEY `telemetry_samples_batch_id_fkey` (`batch_id`),
  KEY `telemetry_samples_query_idx` (`node_id`,`metric`,`sampled_at`),
  CONSTRAINT `telemetry_samples_batch_id_fkey` FOREIGN KEY (`batch_id`) REFERENCES `telemetry_ingest_batches` (`batch_id`) ON DELETE CASCADE,
  CONSTRAINT `telemetry_samples_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `telemetry_samples_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `telemetry_samples_chk_2` CHECK ((length(`batch_id`) = 16)),
  CONSTRAINT `telemetry_samples_telemetry_samples_metric_check` CHECK ((`metric` in (_utf8mb4'cpu_usage_ratio',_utf8mb4'memory_used_bytes',_utf8mb4'network_rx_bytes',_utf8mb4'network_tx_bytes',_utf8mb4'session_count',_utf8mb4'connection_rtt_ms'))),
  CONSTRAINT `telemetry_samples_telemetry_samples_value_check` CHECK ((`value` between -(1.7976931348623157e308) and 1.7976931348623157e308)),
  CONSTRAINT `telemetry_samples_text_no_nul` CHECK ((locate(0x00,cast(`metric` as char charset binary)) = 0)),
  CONSTRAINT `telemetry_samples_time_range` CHECK (((`sampled_at` in (-(9223372036854775808),9223372036854775807)) or ((`sampled_at` >= -(211813488000000000)) and (`sampled_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `telemetry_samples_template` (
  `node_id` varbinary(16) NOT NULL,
  `batch_id` varbinary(16) NOT NULL,
  `sampled_at` bigint NOT NULL,
  `metric` varchar(32) NOT NULL,
  `value` double NOT NULL,
  PRIMARY KEY (`sampled_at`,`node_id`,`batch_id`,`metric`),
  KEY `telemetry_samples_template_query_idx` (`node_id`,`metric`,`sampled_at` DESC),
  KEY `telemetry_samples_template_batch_fk` (`batch_id`),
  CONSTRAINT `telemetry_samples_template_batch_fk` FOREIGN KEY (`batch_id`) REFERENCES `telemetry_ingest_batches` (`batch_id`) ON DELETE CASCADE,
  CONSTRAINT `telemetry_samples_template_node_fk` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `telemetry_samples_template_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `telemetry_samples_template_chk_2` CHECK ((length(`batch_id`) = 16)),
  CONSTRAINT `telemetry_samples_template_chk_3` CHECK ((`value` between -(1.7976931348623157e308) and 1.7976931348623157e308)),
  CONSTRAINT `telemetry_samples_template_chk_4` CHECK ((locate(0x00,cast(`metric` as char charset binary)) = 0)),
  CONSTRAINT `telemetry_samples_template_metric` CHECK ((`metric` in (_utf8mb4'cpu_usage_ratio',_utf8mb4'memory_used_bytes',_utf8mb4'network_rx_bytes',_utf8mb4'network_tx_bytes',_utf8mb4'session_count',_utf8mb4'connection_rtt_ms'))),
  CONSTRAINT `telemetry_samples_template_month` CHECK (((`sampled_at` >= -(211813488000000000)) and (`sampled_at` < 9223371331200000000)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `telemetry_security_events` (
  `event_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `severity` longtext NOT NULL,
  `event_type` longtext NOT NULL,
  `observed_at` bigint NOT NULL,
  `detail` longblob NOT NULL,
  `details_compacted_at` bigint DEFAULT NULL,
  `detail_sha256` varbinary(32) DEFAULT NULL,
  PRIMARY KEY (`event_id`),
  KEY `telemetry_security_node_time_idx` (`node_id`,`observed_at` DESC),
  KEY `security_detail_retention_idx` (`details_compacted_at`,`observed_at`,`event_id`),
  CONSTRAINT `telemetry_security_events_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `security_detail_evidence` CHECK ((((`details_compacted_at` is null) and (`detail_sha256` is null)) or ((`details_compacted_at` is not null) and (`detail_sha256` is not null) and (length(`detail_sha256`) = 32) and (cast(`detail` as char charset binary) = cast(_utf8mb4'{}' as char charset binary))))),
  CONSTRAINT `telemetry_security_events_chk_1` CHECK ((length(`event_id`) = 16)),
  CONSTRAINT `telemetry_security_events_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `telemetry_security_events_telemetry_security_events_c9a774274e56` CHECK ((`severity` in (_utf8mb4'info',_utf8mb4'warning',_utf8mb4'critical'))),
  CONSTRAINT `telemetry_security_events_telemetry_security_events_c9bb7ce96b22` CHECK (((char_length(`event_type`) >= 1) and (char_length(`event_type`) <= 128))),
  CONSTRAINT `telemetry_security_events_text_no_nul` CHECK (((locate(0x00,cast(`severity` as char charset binary)) = 0) and (locate(0x00,cast(`event_type` as char charset binary)) = 0))),
  CONSTRAINT `telemetry_security_events_time_range` CHECK (((`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -211813488000000000) and (`observed_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `transport_events` (
  `event_id` varbinary(16) NOT NULL,
  `ingest_sequence` bigint NOT NULL AUTO_INCREMENT,
  `node_id` varbinary(16) NOT NULL,
  `event_type` longtext NOT NULL,
  `traceparent` longtext NOT NULL,
  `payload` longblob NOT NULL,
  `transport_cursor_valid` tinyint(1) NOT NULL DEFAULT '1',
  `occurred_at` bigint NOT NULL,
  `received_at` bigint NOT NULL DEFAULT (timestampdiff(MICROSECOND,_utf8mb4'2000-01-01',now(6))),
  PRIMARY KEY (`event_id`),
  UNIQUE KEY `transport_events_ingest_sequence_key` (`ingest_sequence`),
  KEY `transport_events_node_id_fkey` (`node_id`),
  CONSTRAINT `transport_events_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `transport_events_chk_1` CHECK ((length(`event_id`) = 16)),
  CONSTRAINT `transport_events_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `transport_events_chk_3` CHECK ((`transport_cursor_valid` in (0,1))),
  CONSTRAINT `transport_events_occurred_at_range` CHECK (((`occurred_at` is null) or (`occurred_at` in (-(9223372036854775808),9223372036854775807)) or ((`occurred_at` >= -(211813488000000000)) and (`occurred_at` < 9223371331200000000)))),
  CONSTRAINT `transport_events_received_at_range` CHECK (((`received_at` is null) or (`received_at` in (-(9223372036854775808),9223372036854775807)) or ((`received_at` >= -(211813488000000000)) and (`received_at` < 9223371331200000000)))),
  CONSTRAINT `transport_events_text_no_nul` CHECK (((locate(0x00,cast(`event_type` as char charset binary)) = 0) and (locate(0x00,cast(`traceparent` as char charset binary)) = 0))),
  CONSTRAINT `transport_events_transport_events_event_type_check` CHECK ((`event_type` in (_utf8mb4'connected',_utf8mb4'disconnected',_utf8mb4'command_result',_utf8mb4'heartbeat',_utf8mb4'error',_utf8mb4'path_changed',_utf8mb4'telemetry',_utf8mb4'simulation_result'))),
  CONSTRAINT `transport_events_transport_events_payload_check` CHECK ((length(`payload`) <= 1048576)),
  CONSTRAINT `transport_events_transport_events_traceparent_check` CHECK (regexp_like(`traceparent`,_utf8mb4'(?-i)\\A00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}\\z'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `user_policy_enforcements` (
  `node_id` varbinary(16) NOT NULL,
  `username` longtext NOT NULL,
  `policy_version` bigint NOT NULL,
  `cause` varchar(191) NOT NULL,
  `source_user_version` bigint NOT NULL,
  `operation_id` varbinary(16) DEFAULT NULL,
  `resulting_user_version` bigint DEFAULT NULL,
  `exact_row_id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `period_start` bigint NOT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`exact_row_id`),
  UNIQUE KEY `exact_row_id_key` (`exact_row_id`),
  KEY `user_policy_enforcements_operation_id_fkey` (`operation_id`),
  KEY `exact_node_lookup` (`node_id`),
  CONSTRAINT `user_policy_enforcements_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `user_policy_enforcements_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `user_policy_enforcements_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `user_policy_enforcements_chk_2` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `user_policy_enforcements_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `user_policy_enforcements_period_start_range` CHECK (((`period_start` is null) or (`period_start` in (-(9223372036854775808),9223372036854775807)) or ((`period_start` >= -(211813488000000000)) and (`period_start` < 9223371331200000000)))),
  CONSTRAINT `user_policy_enforcements_text_no_nul` CHECK (((locate(0x00,cast(`username` as char charset binary)) = 0) and (locate(0x00,cast(`cause` as char charset binary)) = 0))),
  CONSTRAINT `user_policy_enforcements_user_policy_enforcements_cause_check` CHECK ((`cause` in (_utf8mb4'quota',_utf8mb4'expiry',_utf8mb4'quota_reset'))),
  CONSTRAINT `user_policy_enforcements_user_policy_enforcements_check` CHECK (((`operation_id` is null) = (`resulting_user_version` is null))),
  CONSTRAINT `user_policy_enforcements_user_policy_enforcements_p_fc96447b73bf` CHECK ((`policy_version` > 0)),
  CONSTRAINT `user_policy_enforcements_user_policy_enforcements_r_0694bb4e74bc` CHECK ((`resulting_user_version` > 0)),
  CONSTRAINT `user_policy_enforcements_user_policy_enforcements_s_393e81dc42a5` CHECK ((`source_user_version` > 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `user_usage_cursors` (
  `node_id` varbinary(16) NOT NULL,
  `session_id` varchar(257) NOT NULL,
  `username` longtext NOT NULL,
  `rx_bytes` bigint NOT NULL,
  `tx_bytes` bigint NOT NULL,
  `connected_at` bigint NOT NULL,
  `observed_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`,`session_id`,`connected_at`),
  CONSTRAINT `user_usage_cursors_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `user_usage_cursors_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `user_usage_cursors_connected_at_range` CHECK (((`connected_at` is null) or (`connected_at` in (-(9223372036854775808),9223372036854775807)) or ((`connected_at` >= -(211813488000000000)) and (`connected_at` < 9223371331200000000)))),
  CONSTRAINT `user_usage_cursors_observed_at_range` CHECK (((`observed_at` is null) or (`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -(211813488000000000)) and (`observed_at` < 9223371331200000000)))),
  CONSTRAINT `user_usage_cursors_text_no_nul` CHECK (((locate(0x00,cast(`session_id` as char charset binary)) = 0) and (locate(0x00,cast(`username` as char charset binary)) = 0))),
  CONSTRAINT `user_usage_cursors_user_usage_cursors_rx_bytes_check` CHECK ((`rx_bytes` >= 0)),
  CONSTRAINT `user_usage_cursors_user_usage_cursors_session_id_check` CHECK (((char_length(`session_id`) >= 1) and (char_length(`session_id`) <= 256))),
  CONSTRAINT `user_usage_cursors_user_usage_cursors_tx_bytes_check` CHECK ((`tx_bytes` >= 0)),
  CONSTRAINT `user_usage_cursors_user_usage_cursors_username_check` CHECK (regexp_like(`username`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `agent_rollouts` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `target_version` longtext NOT NULL,
  `state` longtext NOT NULL DEFAULT (_utf8mb4'queued'),
  `batch_size` int NOT NULL,
  `stop_on_failure` tinyint(1) NOT NULL DEFAULT '1',
  `reason` longtext NOT NULL,
  `approval_id` varbinary(16) NOT NULL,
  `request_hash` longblob NOT NULL,
  `created_by` varbinary(16) NOT NULL,
  `actor_session_id` varbinary(16) NOT NULL,
  `current_batch` int NOT NULL DEFAULT '0',
  `pause_code` longtext NOT NULL DEFAULT (_utf8mb4''),
  `idempotency_key` varchar(191) NOT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `exclusions` longblob NOT NULL DEFAULT (_utf8mb4'[]'),
  PRIMARY KEY (`id`),
  UNIQUE KEY `agent_rollouts_workspace_id_idempotency_key_key` (`workspace_id`,`idempotency_key`),
  KEY `agent_rollouts_approval_id_fkey` (`approval_id`),
  KEY `agent_rollouts_active_idx` (`created_at`),
  KEY `agent_rollouts_workspace_created_idx` (`workspace_id`,`created_at` DESC),
  CONSTRAINT `agent_rollouts_approval_id_fkey` FOREIGN KEY (`approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `agent_rollouts_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `agent_rollouts_agent_rollouts_batch_size_check` CHECK (((`batch_size` >= 1) and (`batch_size` <= 20))),
  CONSTRAINT `agent_rollouts_agent_rollouts_current_batch_check` CHECK ((`current_batch` >= 0)),
  CONSTRAINT `agent_rollouts_agent_rollouts_idempotency_key_check` CHECK (((char_length(`idempotency_key`) >= 1) and (char_length(`idempotency_key`) <= 128))),
  CONSTRAINT `agent_rollouts_agent_rollouts_pause_code_check` CHECK ((char_length(`pause_code`) <= 128)),
  CONSTRAINT `agent_rollouts_agent_rollouts_reason_check` CHECK (((char_length(`reason`) >= 1) and (char_length(`reason`) <= 512))),
  CONSTRAINT `agent_rollouts_agent_rollouts_request_hash_check` CHECK ((length(`request_hash`) = 32)),
  CONSTRAINT `agent_rollouts_agent_rollouts_state_check` CHECK ((`state` in (_utf8mb4'queued',_utf8mb4'running',_utf8mb4'paused',_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'cancelled'))),
  CONSTRAINT `agent_rollouts_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `agent_rollouts_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `agent_rollouts_chk_3` CHECK ((`stop_on_failure` in (0,1))),
  CONSTRAINT `agent_rollouts_chk_4` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `agent_rollouts_chk_5` CHECK ((length(`created_by`) = 16)),
  CONSTRAINT `agent_rollouts_chk_6` CHECK ((length(`actor_session_id`) = 16)),
  CONSTRAINT `agent_rollouts_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -211813488000000000) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `agent_rollouts_text_no_nul` CHECK (((locate(0x00,cast(`target_version` as char charset binary)) = 0) and (locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`reason` as char charset binary)) = 0) and (locate(0x00,cast(`pause_code` as char charset binary)) = 0) and (locate(0x00,cast(`idempotency_key` as char charset binary)) = 0))),
  CONSTRAINT `agent_rollouts_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -211813488000000000) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `agent_upgrade_operations` (
  `operation_id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `target_version` longtext NOT NULL,
  `package_sha256` longblob NOT NULL,
  `architecture` longtext NOT NULL,
  `from_version` longtext NOT NULL DEFAULT (_utf8mb4''),
  `approval_id` varbinary(16) DEFAULT NULL,
  `state` longtext NOT NULL,
  `scheduled_at` bigint DEFAULT NULL,
  `completed_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`operation_id`),
  KEY `agent_upgrade_operations_workspace_id_fkey` (`workspace_id`),
  KEY `agent_upgrade_operations_pending_idx` (`node_id`),
  KEY `agent_upgrade_operations_operation_idx` (`operation_id`),
  CONSTRAINT `agent_upgrade_operations_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `agent_upgrade_operations_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `agent_upgrade_operations_agent_upgrade_operations_a_77340163cacc` CHECK ((`architecture` in (_utf8mb4'amd64',_utf8mb4'arm64'))),
  CONSTRAINT `agent_upgrade_operations_agent_upgrade_operations_p_87a56fa7b3e2` CHECK ((length(`package_sha256`) = 32)),
  CONSTRAINT `agent_upgrade_operations_agent_upgrade_operations_state_check` CHECK ((`state` in (_utf8mb4'queued',_utf8mb4'accepted',_utf8mb4'running',_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'rolled_back',_utf8mb4'unknown'))),
  CONSTRAINT `agent_upgrade_operations_chk_1` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `agent_upgrade_operations_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `agent_upgrade_operations_chk_3` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `agent_upgrade_operations_chk_4` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `agent_upgrade_operations_completed_at_range` CHECK (((`completed_at` is null) or (`completed_at` in (-(9223372036854775808),9223372036854775807)) or ((`completed_at` >= -(211813488000000000)) and (`completed_at` < 9223371331200000000)))),
  CONSTRAINT `agent_upgrade_operations_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `agent_upgrade_operations_scheduled_at_range` CHECK (((`scheduled_at` is null) or (`scheduled_at` in (-(9223372036854775808),9223372036854775807)) or ((`scheduled_at` >= -(211813488000000000)) and (`scheduled_at` < 9223371331200000000)))),
  CONSTRAINT `agent_upgrade_operations_text_no_nul` CHECK (((locate(0x00,cast(`target_version` as char charset binary)) = 0) and (locate(0x00,cast(`architecture` as char charset binary)) = 0) and (locate(0x00,cast(`from_version` as char charset binary)) = 0) and (locate(0x00,cast(`state` as char charset binary)) = 0))),
  CONSTRAINT `agent_upgrade_operations_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `approval_authority_resources` (
  `approval_id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `resource_type` varchar(191) NOT NULL,
  `resource_id` varbinary(16) NOT NULL,
  PRIMARY KEY (`approval_id`,`resource_type`,`resource_id`),
  KEY `approval_authority_resources_workspace_id_fkey` (`workspace_id`),
  CONSTRAINT `approval_authority_resources_approval_id_fkey` FOREIGN KEY (`approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE CASCADE,
  CONSTRAINT `approval_authority_resources_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `approval_authority_resources_approval_authority_res_1e18f17e7b06` CHECK ((`resource_type` in (_utf8mb4'workspace',_utf8mb4'node',_utf8mb4'resource',_utf8mb4'secret_ref',_utf8mb4'certificate',_utf8mb4'config_plan',_utf8mb4'batch_operation',_utf8mb4'role_binding'))),
  CONSTRAINT `approval_authority_resources_chk_1` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `approval_authority_resources_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `approval_authority_resources_chk_3` CHECK ((length(`resource_id`) = 16)),
  CONSTRAINT `approval_authority_resources_text_no_nul` CHECK ((locate(0x00,cast(`resource_type` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `approval_batch_items` (
  `approval_id` varbinary(16) NOT NULL,
  `item_index` int NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `username` longtext NOT NULL,
  `action` longtext NOT NULL,
  `expected_version` bigint NOT NULL,
  PRIMARY KEY (`approval_id`,`item_index`),
  KEY `approval_batch_items_node_id_fkey` (`node_id`),
  CONSTRAINT `approval_batch_items_approval_id_fkey` FOREIGN KEY (`approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE CASCADE,
  CONSTRAINT `approval_batch_items_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `approval_batch_items_approval_batch_items_action_check` CHECK ((`action` in (_utf8mb4'disable',_utf8mb4'enable'))),
  CONSTRAINT `approval_batch_items_approval_batch_items_expected_version_check` CHECK ((`expected_version` > 0)),
  CONSTRAINT `approval_batch_items_approval_batch_items_item_index_check` CHECK ((`item_index` >= 0)),
  CONSTRAINT `approval_batch_items_approval_batch_items_username_check` CHECK (regexp_like(`username`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z')),
  CONSTRAINT `approval_batch_items_chk_1` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `approval_batch_items_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `approval_batch_items_text_no_nul` CHECK (((locate(0x00,cast(`username` as char charset binary)) = 0) and (locate(0x00,cast(`action` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `audit_events` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `actor_type` longtext NOT NULL,
  `actor_id` longtext NOT NULL,
  `action` longtext NOT NULL,
  `resource_type` longtext NOT NULL,
  `resource_id` varbinary(16) DEFAULT NULL,
  `request_id` longtext NOT NULL,
  `trace_id` longtext,
  `result` longtext NOT NULL,
  `reason` longtext,
  `previous_event_hash` longblob,
  `event_hash` longblob NOT NULL,
  `source_session_id` varbinary(16) DEFAULT NULL,
  `node_id` varbinary(16) DEFAULT NULL,
  `command_id` varbinary(16) DEFAULT NULL,
  `approval_id` varbinary(16) DEFAULT NULL,
  `error_type` longtext,
  `auth_version` smallint NOT NULL DEFAULT '0',
  `event_key_id` longtext,
  `event_mac` longblob,
  `occurred_at` bigint NOT NULL,
  `before_summary` longblob,
  `after_summary` longblob,
  `details_compacted_at` bigint DEFAULT NULL,
  `compaction_key_id` varbinary(128) DEFAULT NULL,
  `compaction_mac` varbinary(32) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `audit_events_approval_id_fkey` (`approval_id`),
  KEY `audit_events_node_id_fkey` (`node_id`),
  KEY `audit_events_workspace_time_idx` (`workspace_id`,`occurred_at` DESC,`id` DESC),
  KEY `audit_detail_retention_idx` (`details_compacted_at`,`occurred_at`,`id`),
  CONSTRAINT `audit_events_approval_id_fkey` FOREIGN KEY (`approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `audit_events_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `audit_events_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `audit_compaction_evidence` CHECK ((((`details_compacted_at` is null) and (`compaction_key_id` is null) and (`compaction_mac` is null)) or ((`details_compacted_at` is not null) and (`compaction_key_id` is not null) and (`compaction_mac` is not null) and (length(`compaction_key_id`) between 1 and 128) and (length(`compaction_mac`) = 32) and (`auth_version` = 1) and (`reason` is null) and (`before_summary` is null) and (`after_summary` is null)))),
  CONSTRAINT `audit_events_audit_event_auth_fields` CHECK ((((`auth_version` = 0) and (`event_key_id` is null) and (`event_mac` is null)) or ((`auth_version` = 1) and (char_length(`event_key_id`) >= 1) and (char_length(`event_key_id`) <= 128) and regexp_like(`event_key_id`,_utf8mb4'(?-i)\\A[A-Za-z0-9._-]+\\z') and (length(`event_mac`) = 32)))),
  CONSTRAINT `audit_events_audit_event_auth_version` CHECK ((`auth_version` in (0,1))),
  CONSTRAINT `audit_events_audit_event_hash_size` CHECK ((length(`event_hash`) = 32)),
  CONSTRAINT `audit_events_audit_events_result_check` CHECK ((`result` in (_utf8mb4'intent',_utf8mb4'succeeded',_utf8mb4'failed'))),
  CONSTRAINT `audit_events_audit_previous_hash_size` CHECK (((`previous_event_hash` is null) or (length(`previous_event_hash`) = 32))),
  CONSTRAINT `audit_events_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `audit_events_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `audit_events_chk_3` CHECK ((length(`resource_id`) = 16)),
  CONSTRAINT `audit_events_chk_4` CHECK ((length(`source_session_id`) = 16)),
  CONSTRAINT `audit_events_chk_5` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `audit_events_chk_6` CHECK ((length(`command_id`) = 16)),
  CONSTRAINT `audit_events_chk_7` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `audit_events_occurred_at_range` CHECK (((`occurred_at` is null) or (`occurred_at` in (-(9223372036854775808),9223372036854775807)) or ((`occurred_at` >= -211813488000000000) and (`occurred_at` < 9223371331200000000)))),
  CONSTRAINT `audit_events_text_no_nul` CHECK (((locate(0x00,cast(`actor_type` as char charset binary)) = 0) and (locate(0x00,cast(`actor_id` as char charset binary)) = 0) and (locate(0x00,cast(`action` as char charset binary)) = 0) and (locate(0x00,cast(`resource_type` as char charset binary)) = 0) and (locate(0x00,cast(`request_id` as char charset binary)) = 0) and (locate(0x00,cast(`trace_id` as char charset binary)) = 0) and (locate(0x00,cast(`result` as char charset binary)) = 0) and (locate(0x00,cast(`reason` as char charset binary)) = 0) and (locate(0x00,cast(`error_type` as char charset binary)) = 0) and (locate(0x00,cast(`event_key_id` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `batch_operation_items` (
  `batch_id` varbinary(16) NOT NULL,
  `item_index` int NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `username` longtext NOT NULL,
  `action` longtext NOT NULL,
  `expected_version` bigint NOT NULL,
  `state` longtext NOT NULL,
  `child_operation_id` varbinary(16) DEFAULT NULL,
  `error_type` longtext,
  `lease_owner` varbinary(16) DEFAULT NULL,
  `lease_until` bigint DEFAULT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`batch_id`,`item_index`),
  KEY `batch_operation_items_child_operation_id_fkey` (`child_operation_id`),
  KEY `batch_operation_items_node_id_fkey` (`node_id`),
  KEY `batch_operation_items_claim_idx` (`updated_at`,`batch_id`,`item_index`),
  CONSTRAINT `batch_operation_items_batch_id_fkey` FOREIGN KEY (`batch_id`) REFERENCES `batch_operations` (`id`) ON DELETE CASCADE,
  CONSTRAINT `batch_operation_items_child_operation_id_fkey` FOREIGN KEY (`child_operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `batch_operation_items_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `batch_operation_items_batch_operation_items_action_check` CHECK ((`action` in (_utf8mb4'disable',_utf8mb4'enable'))),
  CONSTRAINT `batch_operation_items_batch_operation_items_check` CHECK (((`lease_owner` is null) = (`lease_until` is null))),
  CONSTRAINT `batch_operation_items_batch_operation_items_expecte_5e9097a935d2` CHECK ((`expected_version` > 0)),
  CONSTRAINT `batch_operation_items_batch_operation_items_item_index_check` CHECK ((`item_index` >= 0)),
  CONSTRAINT `batch_operation_items_batch_operation_items_state_check` CHECK ((`state` in (_utf8mb4'queued',_utf8mb4'submitting',_utf8mb4'submitted',_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'unknown',_utf8mb4'offline_pending',_utf8mb4'forbidden'))),
  CONSTRAINT `batch_operation_items_batch_operation_items_username_check` CHECK (regexp_like(`username`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z')),
  CONSTRAINT `batch_operation_items_chk_1` CHECK ((length(`batch_id`) = 16)),
  CONSTRAINT `batch_operation_items_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `batch_operation_items_chk_3` CHECK ((length(`child_operation_id`) = 16)),
  CONSTRAINT `batch_operation_items_chk_4` CHECK ((length(`lease_owner`) = 16)),
  CONSTRAINT `batch_operation_items_lease_until_range` CHECK (((`lease_until` is null) or (`lease_until` in (-(9223372036854775808),9223372036854775807)) or ((`lease_until` >= -(211813488000000000)) and (`lease_until` < 9223371331200000000)))),
  CONSTRAINT `batch_operation_items_text_no_nul` CHECK (((locate(0x00,cast(`username` as char charset binary)) = 0) and (locate(0x00,cast(`action` as char charset binary)) = 0) and (locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`error_type` as char charset binary)) = 0))),
  CONSTRAINT `batch_operation_items_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `commands` (
  `id` varbinary(16) NOT NULL,
  `operation_id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `state` varchar(191) NOT NULL,
  `payload_type` longtext NOT NULL,
  `envelope` longblob NOT NULL,
  `idempotency_key` varchar(191) NOT NULL,
  `expected_version` bigint NOT NULL,
  `sequence` bigint NOT NULL DEFAULT '1',
  `traceparent` longtext NOT NULL,
  `resource_type` varchar(191) DEFAULT NULL,
  `resource_key` longtext,
  `expires_at` bigint NOT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `details_compacted_at` bigint DEFAULT NULL,
  `envelope_sha256` varbinary(32) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `commands_operation_id_key` (`operation_id`),
  UNIQUE KEY `commands_workspace_id_idempotency_key_key` (`workspace_id`,`idempotency_key`),
  KEY `commands_workspace_id_node_id_fkey` (`workspace_id`,`node_id`),
  KEY `commands_node_state_idx` (`node_id`,`state`,`created_at`,`id`),
  KEY `commands_pending_resource_idx` (`node_id`,`resource_type`,`created_at`),
  KEY `commands_retention_idx` (`details_compacted_at`,`updated_at`,`id`),
  CONSTRAINT `commands_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `commands_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `commands_workspace_id_node_id_fkey` FOREIGN KEY (`workspace_id`, `node_id`) REFERENCES `nodes` (`workspace_id`, `id`) ON DELETE RESTRICT,
  CONSTRAINT `commands_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `commands_chk_2` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `commands_chk_3` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `commands_chk_4` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `commands_commands_check` CHECK ((`expires_at` > `created_at`)),
  CONSTRAINT `commands_commands_envelope_check` CHECK (((length(`envelope`) >= 1) and (length(`envelope`) <= 1048576))),
  CONSTRAINT `commands_commands_expected_version_check` CHECK ((`expected_version` >= 0)),
  CONSTRAINT `commands_commands_idempotency_key_check` CHECK (((char_length(`idempotency_key`) >= 1) and (char_length(`idempotency_key`) <= 128))),
  CONSTRAINT `commands_commands_payload_type_check` CHECK ((`payload_type` in (_utf8mb4'synthetic_noop',_utf8mb4'synthetic_echo',_utf8mb4'session_disconnect',_utf8mb4'session_terminate',_utf8mb4'ip_ban_remove',_utf8mb4'service_reload',_utf8mb4'user_create',_utf8mb4'user_disable',_utf8mb4'user_enable',_utf8mb4'user_password_rotate',_utf8mb4'group_apply',_utf8mb4'config_plan',_utf8mb4'config_apply',_utf8mb4'certificate_csr',_utf8mb4'certificate_p12',_utf8mb4'certificate_revoke',_utf8mb4'agent_upgrade'))),
  CONSTRAINT `commands_commands_resource_identity` CHECK ((((`resource_type` is null) = (`resource_key` is null)) and ((`resource_type` is null) or (`resource_type` in (_utf8mb4'user',_utf8mb4'group'))))),
  CONSTRAINT `commands_commands_sequence_check` CHECK ((`sequence` > 0)),
  CONSTRAINT `commands_commands_state_check` CHECK ((`state` in (_utf8mb4'queued',_utf8mb4'dispatched',_utf8mb4'accepted',_utf8mb4'running',_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'rejected',_utf8mb4'unknown',_utf8mb4'expired',_utf8mb4'rolled_back',_utf8mb4'superseded'))),
  CONSTRAINT `commands_commands_traceparent_check` CHECK (regexp_like(`traceparent`,_utf8mb4'(?-i)\\A00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}\\z')),
  CONSTRAINT `commands_compaction_evidence` CHECK ((((`details_compacted_at` is null) and (`envelope_sha256` is null)) or ((`details_compacted_at` is not null) and (`envelope_sha256` is not null) and (length(`envelope_sha256`) = 32) and (`state` in (_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'rejected',_utf8mb4'expired',_utf8mb4'rolled_back',_utf8mb4'superseded'))))),
  CONSTRAINT `commands_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -211813488000000000) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `commands_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -211813488000000000) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `commands_text_no_nul` CHECK (((locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`payload_type` as char charset binary)) = 0) and (locate(0x00,cast(`idempotency_key` as char charset binary)) = 0) and (locate(0x00,cast(`traceparent` as char charset binary)) = 0) and (locate(0x00,cast(`resource_type` as char charset binary)) = 0) and (locate(0x00,cast(`resource_key` as char charset binary)) = 0))),
  CONSTRAINT `commands_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -211813488000000000) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `config_plans` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `operation_id` varbinary(16) NOT NULL,
  `template_name` longtext NOT NULL,
  `expected_revision` bigint NOT NULL,
  `candidate_hash` longblob NOT NULL,
  `candidate_redacted` longtext NOT NULL,
  `created_by` varbinary(16) DEFAULT NULL,
  `expires_at` bigint NOT NULL,
  `created_at` bigint NOT NULL,
  `warnings` longblob NOT NULL DEFAULT (_utf8mb4'[]'),
  PRIMARY KEY (`id`),
  UNIQUE KEY `config_plans_operation_id_key` (`operation_id`),
  KEY `config_plans_created_by_fkey` (`created_by`),
  KEY `config_plans_workspace_id_node_id_fkey` (`workspace_id`,`node_id`),
  KEY `config_plans_node_created_idx` (`node_id`,`created_at` DESC,`id` DESC),
  CONSTRAINT `config_plans_created_by_fkey` FOREIGN KEY (`created_by`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_plans_id_fkey` FOREIGN KEY (`id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_plans_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_plans_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_plans_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_plans_workspace_id_node_id_fkey` FOREIGN KEY (`workspace_id`, `node_id`) REFERENCES `nodes` (`workspace_id`, `id`) ON DELETE RESTRICT,
  CONSTRAINT `config_plans_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `config_plans_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `config_plans_chk_3` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `config_plans_chk_4` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `config_plans_chk_5` CHECK ((length(`created_by`) = 16)),
  CONSTRAINT `config_plans_config_plans_candidate_hash_check` CHECK ((length(`candidate_hash`) = 32)),
  CONSTRAINT `config_plans_config_plans_candidate_redacted_check` CHECK (((length(`candidate_redacted`) >= 1) and (length(`candidate_redacted`) <= 262144))),
  CONSTRAINT `config_plans_config_plans_expected_revision_check` CHECK ((`expected_revision` >= 0)),
  CONSTRAINT `config_plans_config_plans_template_name_check` CHECK (((char_length(`template_name`) >= 1) and (char_length(`template_name`) <= 128))),
  CONSTRAINT `config_plans_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -211813488000000000) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `config_plans_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -211813488000000000) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `config_plans_text_no_nul` CHECK (((locate(0x00,cast(`template_name` as char charset binary)) = 0) and (locate(0x00,cast(`candidate_redacted` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `desired_groups` (
  `node_id` varbinary(16) NOT NULL,
  `group_name` varchar(191) NOT NULL,
  `version` bigint NOT NULL,
  `revision` bigint NOT NULL,
  `fingerprint` longblob NOT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `members` longblob NOT NULL,
  PRIMARY KEY (`node_id`,`group_name`),
  CONSTRAINT `desired_groups_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `desired_groups_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `desired_groups_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -211813488000000000) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `desired_groups_desired_groups_fingerprint_check` CHECK ((length(`fingerprint`) = 32)),
  CONSTRAINT `desired_groups_desired_groups_group_name_check` CHECK (regexp_like(`group_name`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z')),
  CONSTRAINT `desired_groups_desired_groups_revision_check` CHECK ((`revision` > 0)),
  CONSTRAINT `desired_groups_desired_groups_version_check` CHECK ((`version` > 0)),
  CONSTRAINT `desired_groups_text_no_nul` CHECK ((locate(0x00,cast(`group_name` as char charset binary)) = 0)),
  CONSTRAINT `desired_groups_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -211813488000000000) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `desired_users` (
  `node_id` varbinary(16) NOT NULL,
  `username` varchar(191) NOT NULL,
  `enabled` tinyint(1) NOT NULL,
  `version` bigint NOT NULL,
  `revision` bigint NOT NULL,
  `fingerprint` longblob NOT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`,`username`),
  CONSTRAINT `desired_users_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `desired_users_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `desired_users_chk_2` CHECK ((`enabled` in (0,1))),
  CONSTRAINT `desired_users_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `desired_users_desired_users_fingerprint_check` CHECK ((length(`fingerprint`) = 32)),
  CONSTRAINT `desired_users_desired_users_revision_check` CHECK ((`revision` > 0)),
  CONSTRAINT `desired_users_desired_users_username_check` CHECK (regexp_like(`username`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z')),
  CONSTRAINT `desired_users_desired_users_version_check` CHECK ((`version` > 0)),
  CONSTRAINT `desired_users_text_no_nul` CHECK ((locate(0x00,cast(`username` as char charset binary)) = 0)),
  CONSTRAINT `desired_users_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `enrollment_tokens` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `token_hash` varbinary(512) NOT NULL,
  `expected_environment` longtext NOT NULL,
  `expected_node_name` longtext,
  `expected_endpoint_id` longblob,
  `consumed_node_id` varbinary(16) DEFAULT NULL,
  `created_by` longtext NOT NULL,
  `expires_at` bigint NOT NULL,
  `consumed_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `enrollment_tokens_token_hash_key` (`token_hash`),
  KEY `enrollment_tokens_workspace_id_consumed_node_id_fkey` (`workspace_id`,`consumed_node_id`),
  KEY `enrollment_tokens_workspace_expiry_idx` (`workspace_id`,`expires_at` DESC),
  CONSTRAINT `enrollment_tokens_workspace_id_consumed_node_id_fkey` FOREIGN KEY (`workspace_id`, `consumed_node_id`) REFERENCES `nodes` (`workspace_id`, `id`) ON DELETE RESTRICT,
  CONSTRAINT `enrollment_tokens_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `enrollment_tokens_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `enrollment_tokens_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `enrollment_tokens_chk_3` CHECK ((length(`consumed_node_id`) = 16)),
  CONSTRAINT `enrollment_tokens_consumed_at_range` CHECK (((`consumed_at` is null) or (`consumed_at` in (-(9223372036854775808),9223372036854775807)) or ((`consumed_at` >= -(211813488000000000)) and (`consumed_at` < 9223371331200000000)))),
  CONSTRAINT `enrollment_tokens_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `enrollment_tokens_enrollment_tokens_check` CHECK ((`expires_at` > `created_at`)),
  CONSTRAINT `enrollment_tokens_enrollment_tokens_check1` CHECK (((`consumed_at` is null) = (`consumed_node_id` is null))),
  CONSTRAINT `enrollment_tokens_enrollment_tokens_created_by_check` CHECK (((char_length(`created_by`) >= 1) and (char_length(`created_by`) <= 256))),
  CONSTRAINT `enrollment_tokens_enrollment_tokens_expected_endpoint_id_check` CHECK (((`expected_endpoint_id` is null) or (length(`expected_endpoint_id`) = 32))),
  CONSTRAINT `enrollment_tokens_enrollment_tokens_expected_environment_check` CHECK (((char_length(`expected_environment`) >= 1) and (char_length(`expected_environment`) <= 64))),
  CONSTRAINT `enrollment_tokens_enrollment_tokens_expected_node_name_check` CHECK (((`expected_node_name` is null) or ((char_length(`expected_node_name`) >= 1) and (char_length(`expected_node_name`) <= 128)))),
  CONSTRAINT `enrollment_tokens_enrollment_tokens_token_hash_check` CHECK ((length(`token_hash`) = 32)),
  CONSTRAINT `enrollment_tokens_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -(211813488000000000)) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `enrollment_tokens_text_no_nul` CHECK (((locate(0x00,cast(`expected_environment` as char charset binary)) = 0) and (locate(0x00,cast(`expected_node_name` as char charset binary)) = 0) and (locate(0x00,cast(`created_by` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `exact_nodes` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_nodes_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `exact_operations` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_operations_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `operations` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `exact_telemetry_rollups_5m` (
  `owner_id` bigint unsigned NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  KEY `telemetry_key_lookup` (`key_value`(255)),
  CONSTRAINT `exact_telemetry_rollups_5m_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `telemetry_rollups_5m` (`exact_row_id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `exact_user_policy_enforcements` (
  `owner_id` bigint unsigned NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_user_policy_enforcements_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `user_policy_enforcements` (`exact_row_id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `local_slice_jobs` (
  `operation_id` varbinary(16) NOT NULL,
  `command_envelope` longblob NOT NULL,
  `traceparent` longtext NOT NULL,
  `attempts` int NOT NULL DEFAULT '0',
  `last_error` longtext,
  `available_at` bigint NOT NULL,
  `expires_at` bigint NOT NULL,
  `dispatched_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`operation_id`),
  KEY `local_slice_jobs_dispatch_idx` (`available_at`,`operation_id`),
  CONSTRAINT `local_slice_jobs_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE CASCADE,
  CONSTRAINT `local_slice_jobs_available_at_range` CHECK (((`available_at` is null) or (`available_at` in (-(9223372036854775808),9223372036854775807)) or ((`available_at` >= -(211813488000000000)) and (`available_at` < 9223371331200000000)))),
  CONSTRAINT `local_slice_jobs_chk_1` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `local_slice_jobs_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `local_slice_jobs_dispatched_at_range` CHECK (((`dispatched_at` is null) or (`dispatched_at` in (-(9223372036854775808),9223372036854775807)) or ((`dispatched_at` >= -(211813488000000000)) and (`dispatched_at` < 9223371331200000000)))),
  CONSTRAINT `local_slice_jobs_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -(211813488000000000)) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `local_slice_jobs_local_slice_jobs_attempts_check` CHECK ((`attempts` >= 0)),
  CONSTRAINT `local_slice_jobs_local_slice_jobs_command_envelope_check` CHECK (((length(`command_envelope`) >= 1) and (length(`command_envelope`) <= 1048576))),
  CONSTRAINT `local_slice_jobs_local_slice_jobs_traceparent_check` CHECK (regexp_like(`traceparent`,_utf8mb4'(?-i)\\A00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}\\z')),
  CONSTRAINT `local_slice_jobs_text_no_nul` CHECK (((locate(0x00,cast(`traceparent` as char charset binary)) = 0) and (locate(0x00,cast(`last_error` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_bootstrap_tokens` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `token_hash` varbinary(512) NOT NULL,
  `expected_environment` longtext NOT NULL,
  `expected_node_name` longtext,
  `bound_endpoint_id` longblob,
  `consumed_node_id` varbinary(16) DEFAULT NULL,
  `created_by` longtext NOT NULL,
  `expires_at` bigint NOT NULL,
  `consumed_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `expected_endpoint_id` varbinary(32) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `node_bootstrap_tokens_token_hash_key` (`token_hash`),
  KEY `node_bootstrap_tokens_workspace_id_consumed_node_id_fkey` (`workspace_id`,`consumed_node_id`),
  KEY `node_bootstrap_tokens_workspace_expiry_idx` (`workspace_id`,`expires_at` DESC),
  CONSTRAINT `node_bootstrap_tokens_workspace_id_consumed_node_id_fkey` FOREIGN KEY (`workspace_id`, `consumed_node_id`) REFERENCES `nodes` (`workspace_id`, `id`) ON DELETE RESTRICT,
  CONSTRAINT `node_bootstrap_tokens_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `node_bootstrap_tokens_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `node_bootstrap_tokens_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `node_bootstrap_tokens_chk_3` CHECK ((length(`consumed_node_id`) = 16)),
  CONSTRAINT `node_bootstrap_tokens_chk_4` CHECK (((`expected_endpoint_id` is null) or (length(`expected_endpoint_id`) = 32))),
  CONSTRAINT `node_bootstrap_tokens_consumed_at_range` CHECK (((`consumed_at` is null) or (`consumed_at` in (-(9223372036854775808),9223372036854775807)) or ((`consumed_at` >= -211813488000000000) and (`consumed_at` < 9223371331200000000)))),
  CONSTRAINT `node_bootstrap_tokens_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -211813488000000000) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `node_bootstrap_tokens_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -211813488000000000) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `node_bootstrap_tokens_node_bootstrap_tokens_bound_e_5998dcbaa1c3` CHECK (((`bound_endpoint_id` is null) or (length(`bound_endpoint_id`) = 32))),
  CONSTRAINT `node_bootstrap_tokens_node_bootstrap_tokens_check` CHECK ((`expires_at` > `created_at`)),
  CONSTRAINT `node_bootstrap_tokens_node_bootstrap_tokens_check1` CHECK (((`bound_endpoint_id` is null) = (`consumed_at` is null))),
  CONSTRAINT `node_bootstrap_tokens_node_bootstrap_tokens_check2` CHECK (((`consumed_at` is null) = (`consumed_node_id` is null))),
  CONSTRAINT `node_bootstrap_tokens_node_bootstrap_tokens_created_by_check` CHECK (((char_length(`created_by`) >= 1) and (char_length(`created_by`) <= 256))),
  CONSTRAINT `node_bootstrap_tokens_node_bootstrap_tokens_expecte_4d6a3a880b58` CHECK (((char_length(`expected_environment`) >= 1) and (char_length(`expected_environment`) <= 64))),
  CONSTRAINT `node_bootstrap_tokens_node_bootstrap_tokens_expecte_ebe912456bc1` CHECK (((`expected_node_name` is null) or ((char_length(`expected_node_name`) >= 1) and (char_length(`expected_node_name`) <= 128)))),
  CONSTRAINT `node_bootstrap_tokens_node_bootstrap_tokens_token_hash_check` CHECK ((length(`token_hash`) = 32)),
  CONSTRAINT `node_bootstrap_tokens_text_no_nul` CHECK (((locate(0x00,cast(`expected_environment` as char charset binary)) = 0) and (locate(0x00,cast(`expected_node_name` as char charset binary)) = 0) and (locate(0x00,cast(`created_by` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_capabilities` (
  `node_id` varbinary(16) NOT NULL,
  `capability` varchar(191) NOT NULL,
  `approved` tinyint(1) NOT NULL DEFAULT '0',
  PRIMARY KEY (`node_id`,`capability`),
  CONSTRAINT `node_capabilities_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `node_capabilities_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_capabilities_chk_2` CHECK ((`approved` in (0,1))),
  CONSTRAINT `node_capabilities_node_capabilities_capability_check` CHECK (((char_length(`capability`) >= 1) and (char_length(`capability`) <= 128))),
  CONSTRAINT `node_capabilities_text_no_nul` CHECK ((locate(0x00,cast(`capability` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_command_leases` (
  `node_id` varbinary(16) NOT NULL,
  `command_id` varbinary(16) NOT NULL,
  `lease_token` varbinary(16) NOT NULL,
  `worker_id` varbinary(16) NOT NULL,
  `leased_until` bigint NOT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`),
  UNIQUE KEY `node_command_leases_command_id_key` (`command_id`),
  UNIQUE KEY `node_command_leases_lease_token_key` (`lease_token`),
  KEY `node_command_leases_expiry_idx` (`leased_until`),
  CONSTRAINT `node_command_leases_command_id_fkey` FOREIGN KEY (`command_id`) REFERENCES `commands` (`id`) ON DELETE CASCADE,
  CONSTRAINT `node_command_leases_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `node_command_leases_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_command_leases_chk_2` CHECK ((length(`command_id`) = 16)),
  CONSTRAINT `node_command_leases_chk_3` CHECK ((length(`lease_token`) = 16)),
  CONSTRAINT `node_command_leases_chk_4` CHECK ((length(`worker_id`) = 16)),
  CONSTRAINT `node_command_leases_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `node_command_leases_leased_until_range` CHECK (((`leased_until` is null) or (`leased_until` in (-(9223372036854775808),9223372036854775807)) or ((`leased_until` >= -(211813488000000000)) and (`leased_until` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_config_state` (
  `node_id` varbinary(16) NOT NULL,
  `revision` bigint NOT NULL DEFAULT '0',
  `candidate_hash` longblob,
  `redacted_config` longtext NOT NULL DEFAULT (_utf8mb4''),
  `automation_locked` tinyint(1) NOT NULL DEFAULT '0',
  `automation_lock_reason` longtext,
  `last_apply_operation_id` varbinary(16) DEFAULT NULL,
  `desired_revision` bigint NOT NULL DEFAULT '0',
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`),
  KEY `node_config_state_last_apply_operation_id_fkey` (`last_apply_operation_id`),
  CONSTRAINT `node_config_state_last_apply_operation_id_fkey` FOREIGN KEY (`last_apply_operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `node_config_state_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `node_config_state_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_config_state_chk_2` CHECK ((`automation_locked` in (0,1))),
  CONSTRAINT `node_config_state_chk_3` CHECK ((length(`last_apply_operation_id`) = 16)),
  CONSTRAINT `node_config_state_node_config_state_automation_lock_reason_check` CHECK (((`automation_lock_reason` is null) or ((char_length(`automation_lock_reason`) >= 1) and (char_length(`automation_lock_reason`) <= 128)))),
  CONSTRAINT `node_config_state_node_config_state_candidate_hash_check` CHECK (((`candidate_hash` is null) or (length(`candidate_hash`) = 32))),
  CONSTRAINT `node_config_state_node_config_state_check` CHECK ((`desired_revision` >= `revision`)),
  CONSTRAINT `node_config_state_node_config_state_redacted_config_check` CHECK ((length(`redacted_config`) <= 262144)),
  CONSTRAINT `node_config_state_node_config_state_revision_check` CHECK ((`revision` >= 0)),
  CONSTRAINT `node_config_state_text_no_nul` CHECK (((locate(0x00,cast(`redacted_config` as char charset binary)) = 0) and (locate(0x00,cast(`automation_lock_reason` as char charset binary)) = 0))),
  CONSTRAINT `node_config_state_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_endpoint_keys` (
  `node_id` varbinary(16) NOT NULL,
  `endpoint_id` varbinary(512) NOT NULL,
  `state` longtext NOT NULL,
  `bound_at` bigint NOT NULL,
  `revoked_at` bigint DEFAULT NULL,
  PRIMARY KEY (`node_id`),
  UNIQUE KEY `node_endpoint_keys_endpoint_id_key` (`endpoint_id`),
  CONSTRAINT `node_endpoint_keys_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `node_endpoint_keys_bound_at_range` CHECK (((`bound_at` is null) or (`bound_at` in (-(9223372036854775808),9223372036854775807)) or ((`bound_at` >= -(211813488000000000)) and (`bound_at` < 9223371331200000000)))),
  CONSTRAINT `node_endpoint_keys_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_endpoint_keys_node_endpoint_keys_check` CHECK (((`state` = _utf8mb4'revoked') = (`revoked_at` is not null))),
  CONSTRAINT `node_endpoint_keys_node_endpoint_keys_endpoint_id_check` CHECK ((length(`endpoint_id`) = 32)),
  CONSTRAINT `node_endpoint_keys_node_endpoint_keys_state_check` CHECK ((`state` in (_utf8mb4'pending',_utf8mb4'active',_utf8mb4'revoked'))),
  CONSTRAINT `node_endpoint_keys_revoked_at_range` CHECK (((`revoked_at` is null) or (`revoked_at` in (-(9223372036854775808),9223372036854775807)) or ((`revoked_at` >= -(211813488000000000)) and (`revoked_at` < 9223371331200000000)))),
  CONSTRAINT `node_endpoint_keys_text_no_nul` CHECK ((locate(0x00,cast(`state` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_ip_bans` (
  `node_id` varbinary(16) NOT NULL,
  `ip` varbinary(18) NOT NULL,
  `seconds_remaining` bigint DEFAULT NULL,
  `observed_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`,`ip`),
  CONSTRAINT `node_ip_bans_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `node_ip_bans_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_ip_bans_chk_2` CHECK (((`ip` is null) or ((length(`ip`) = 6) and (ord(substr(`ip`,1,1)) = 4) and (ord(substr(`ip`,2,1)) <= 32)) or ((length(`ip`) = 18) and (ord(substr(`ip`,1,1)) = 6) and (ord(substr(`ip`,2,1)) <= 128)))),
  CONSTRAINT `node_ip_bans_node_ip_bans_seconds_remaining_check` CHECK (((`seconds_remaining` is null) or (`seconds_remaining` >= 0))),
  CONSTRAINT `node_ip_bans_observed_at_range` CHECK (((`observed_at` is null) or (`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -(211813488000000000)) and (`observed_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_observed_snapshots` (
  `node_id` varbinary(16) NOT NULL,
  `boot_id` longtext NOT NULL,
  `agent_instance_id` varbinary(16) NOT NULL,
  `agent_version` longtext NOT NULL,
  `ocserv_version` longtext NOT NULL,
  `os_release` longtext NOT NULL,
  `dropped_security` bigint NOT NULL DEFAULT '0',
  `dropped_health` bigint NOT NULL DEFAULT '0',
  `dropped_aggregate` bigint NOT NULL DEFAULT '0',
  `dropped_raw` bigint NOT NULL DEFAULT '0',
  `architecture` longtext NOT NULL DEFAULT (_utf8mb4''),
  `observed_at` bigint NOT NULL,
  `received_at` bigint NOT NULL DEFAULT (timestampdiff(MICROSECOND,_utf8mb4'2000-01-01 00:00:00',now(6))),
  `last_heartbeat_at` bigint NOT NULL,
  `ocserv` longblob NOT NULL,
  `system` longblob NOT NULL,
  `path` longblob NOT NULL,
  PRIMARY KEY (`node_id`),
  KEY `node_observed_freshness_idx` (`last_heartbeat_at`),
  CONSTRAINT `node_observed_snapshots_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `node_observed_snapshots_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_observed_snapshots_chk_2` CHECK ((length(`agent_instance_id`) = 16)),
  CONSTRAINT `node_observed_snapshots_last_heartbeat_at_range` CHECK (((`last_heartbeat_at` is null) or (`last_heartbeat_at` in (-(9223372036854775808),9223372036854775807)) or ((`last_heartbeat_at` >= -211813488000000000) and (`last_heartbeat_at` < 9223371331200000000)))),
  CONSTRAINT `node_observed_snapshots_node_observed_snapshots_age_2f74175e6a1f` CHECK (((char_length(`agent_version`) >= 1) and (char_length(`agent_version`) <= 128))),
  CONSTRAINT `node_observed_snapshots_node_observed_snapshots_boot_id_check` CHECK (((char_length(`boot_id`) >= 1) and (char_length(`boot_id`) <= 128))),
  CONSTRAINT `node_observed_snapshots_node_observed_snapshots_dro_17664e5dfecb` CHECK ((`dropped_security` >= 0)),
  CONSTRAINT `node_observed_snapshots_node_observed_snapshots_dro_2de7f37e70b2` CHECK ((`dropped_aggregate` >= 0)),
  CONSTRAINT `node_observed_snapshots_node_observed_snapshots_dro_e614d16434b3` CHECK ((`dropped_raw` >= 0)),
  CONSTRAINT `node_observed_snapshots_node_observed_snapshots_dro_ff73a1f9e5b1` CHECK ((`dropped_health` >= 0)),
  CONSTRAINT `node_observed_snapshots_node_observed_snapshots_ocs_91217ea24665` CHECK (((char_length(`ocserv_version`) >= 1) and (char_length(`ocserv_version`) <= 128))),
  CONSTRAINT `node_observed_snapshots_node_observed_snapshots_os_release_check` CHECK (((char_length(`os_release`) >= 1) and (char_length(`os_release`) <= 128))),
  CONSTRAINT `node_observed_snapshots_observed_at_range` CHECK (((`observed_at` is null) or (`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -211813488000000000) and (`observed_at` < 9223371331200000000)))),
  CONSTRAINT `node_observed_snapshots_received_at_range` CHECK (((`received_at` is null) or (`received_at` in (-(9223372036854775808),9223372036854775807)) or ((`received_at` >= -211813488000000000) and (`received_at` < 9223371331200000000)))),
  CONSTRAINT `node_observed_snapshots_text_no_nul` CHECK (((locate(0x00,cast(`boot_id` as char charset binary)) = 0) and (locate(0x00,cast(`agent_version` as char charset binary)) = 0) and (locate(0x00,cast(`ocserv_version` as char charset binary)) = 0) and (locate(0x00,cast(`os_release` as char charset binary)) = 0) and (locate(0x00,cast(`architecture` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_privd_attestation_keys` (
  `node_id` varbinary(16) NOT NULL,
  `key_id` varchar(191) NOT NULL,
  `algorithm` longtext NOT NULL,
  `public_key` varbinary(512) NOT NULL,
  `state` varchar(191) NOT NULL,
  `predecessor_key_id` longtext,
  `successor_key_id` longtext,
  `registration_credential_id` varbinary(16) NOT NULL,
  `created_at` bigint NOT NULL,
  `approved_at` bigint NOT NULL,
  `activated_at` bigint NOT NULL,
  `valid_until` bigint DEFAULT NULL,
  `revoked_at` bigint DEFAULT NULL,
  PRIMARY KEY (`node_id`,`key_id`),
  UNIQUE KEY `node_privd_attestation_keys_key_id_key` (`key_id`),
  UNIQUE KEY `node_privd_attestation_keys_public_key_key` (`public_key`),
  UNIQUE KEY `node_privd_attestation_keys_registration_credential_id_key` (`registration_credential_id`),
  KEY `node_privd_attestation_keys_active_idx` (`node_id`,`state`,`activated_at`),
  CONSTRAINT `node_privd_attestation_keys_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `node_privd_attestation_keys_registration_credential_id_fkey` FOREIGN KEY (`registration_credential_id`) REFERENCES `privd_attestation_enrollment_credentials` (`id`),
  CONSTRAINT `node_privd_attestation_keys_activated_at_range` CHECK (((`activated_at` is null) or (`activated_at` in (-(9223372036854775808),9223372036854775807)) or ((`activated_at` >= -(211813488000000000)) and (`activated_at` < 9223371331200000000)))),
  CONSTRAINT `node_privd_attestation_keys_approved_at_range` CHECK (((`approved_at` is null) or (`approved_at` in (-(9223372036854775808),9223372036854775807)) or ((`approved_at` >= -(211813488000000000)) and (`approved_at` < 9223371331200000000)))),
  CONSTRAINT `node_privd_attestation_keys_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_privd_attestation_keys_chk_2` CHECK ((length(`registration_credential_id`) = 16)),
  CONSTRAINT `node_privd_attestation_keys_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `node_privd_attestation_keys_node_privd_attestation__57a7fe3617d1` CHECK (regexp_like(`key_id`,_utf8mb4'(?-i)\\Aed25519-sha256:[0-9a-f]{64}\\z')),
  CONSTRAINT `node_privd_attestation_keys_node_privd_attestation__af8dc9110f78` CHECK ((`state` in (_utf8mb4'pending',_utf8mb4'active',_utf8mb4'revoked'))),
  CONSTRAINT `node_privd_attestation_keys_node_privd_attestation__bcd0db2625e4` CHECK ((`algorithm` = _utf8mb4'ed25519')),
  CONSTRAINT `node_privd_attestation_keys_node_privd_attestation__e995472f8370` CHECK ((length(`public_key`) = 32)),
  CONSTRAINT `node_privd_attestation_keys_node_privd_attestation_keys_check` CHECK (((`valid_until` is null) or (`valid_until` >= `activated_at`))),
  CONSTRAINT `node_privd_attestation_keys_node_privd_attestation_keys_check1` CHECK (((`state` = _utf8mb4'revoked') = (`revoked_at` is not null))),
  CONSTRAINT `node_privd_attestation_keys_node_privd_attestation_keys_check2` CHECK (((`predecessor_key_id` is null) or (`predecessor_key_id` <> `key_id`))),
  CONSTRAINT `node_privd_attestation_keys_node_privd_attestation_keys_check3` CHECK (((`successor_key_id` is null) or (`successor_key_id` <> `key_id`))),
  CONSTRAINT `node_privd_attestation_keys_revoked_at_range` CHECK (((`revoked_at` is null) or (`revoked_at` in (-(9223372036854775808),9223372036854775807)) or ((`revoked_at` >= -(211813488000000000)) and (`revoked_at` < 9223371331200000000)))),
  CONSTRAINT `node_privd_attestation_keys_text_no_nul` CHECK (((locate(0x00,cast(`key_id` as char charset binary)) = 0) and (locate(0x00,cast(`algorithm` as char charset binary)) = 0) and (locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`predecessor_key_id` as char charset binary)) = 0) and (locate(0x00,cast(`successor_key_id` as char charset binary)) = 0))),
  CONSTRAINT `node_privd_attestation_keys_valid_until_range` CHECK (((`valid_until` is null) or (`valid_until` in (-(9223372036854775808),9223372036854775807)) or ((`valid_until` >= -(211813488000000000)) and (`valid_until` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_sealing_keys` (
  `node_id` varbinary(16) NOT NULL,
  `purpose` smallint NOT NULL,
  `version` smallint NOT NULL,
  `key_id` varchar(191) NOT NULL,
  `public_key_sha256` varbinary(512) NOT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`,`purpose`),
  UNIQUE KEY `node_sealing_keys_node_id_key_id_key` (`node_id`,`key_id`),
  UNIQUE KEY `node_sealing_keys_node_id_public_key_sha256_key` (`node_id`,`public_key_sha256`),
  CONSTRAINT `node_sealing_keys_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `node_sealing_keys_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_sealing_keys_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `node_sealing_keys_node_sealing_keys_key_id_check` CHECK (regexp_like(`key_id`,_utf8mb4'(?-i)\\A[A-Za-z0-9_.-]{1,128}\\z')),
  CONSTRAINT `node_sealing_keys_node_sealing_keys_public_key_sha256_check` CHECK ((length(`public_key_sha256`) = 32)),
  CONSTRAINT `node_sealing_keys_node_sealing_keys_purpose_check` CHECK ((`purpose` in (1,2))),
  CONSTRAINT `node_sealing_keys_node_sealing_keys_version_check` CHECK ((`version` = 1)),
  CONSTRAINT `node_sealing_keys_text_no_nul` CHECK ((locate(0x00,cast(`key_id` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_sessions` (
  `node_id` varbinary(16) NOT NULL,
  `session_id` varchar(257) NOT NULL,
  `username` longtext NOT NULL,
  `client_ip` varbinary(18) NOT NULL,
  `bytes_in` bigint NOT NULL,
  `bytes_out` bigint NOT NULL,
  `connected_at` bigint NOT NULL,
  `observed_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`,`session_id`),
  KEY `node_sessions_observed_idx` (`node_id`,`observed_at` DESC,`session_id`),
  CONSTRAINT `node_sessions_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `node_sessions_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_sessions_chk_2` CHECK (((`client_ip` is null) or ((length(`client_ip`) = 6) and (ord(substr(`client_ip`,1,1)) = 4) and (ord(substr(`client_ip`,2,1)) <= 32)) or ((length(`client_ip`) = 18) and (ord(substr(`client_ip`,1,1)) = 6) and (ord(substr(`client_ip`,2,1)) <= 128)))),
  CONSTRAINT `node_sessions_connected_at_range` CHECK (((`connected_at` is null) or (`connected_at` in (-(9223372036854775808),9223372036854775807)) or ((`connected_at` >= -(211813488000000000)) and (`connected_at` < 9223371331200000000)))),
  CONSTRAINT `node_sessions_node_sessions_bytes_in_check` CHECK ((`bytes_in` >= 0)),
  CONSTRAINT `node_sessions_node_sessions_bytes_out_check` CHECK ((`bytes_out` >= 0)),
  CONSTRAINT `node_sessions_node_sessions_session_id_check` CHECK (((char_length(`session_id`) >= 1) and (char_length(`session_id`) <= 256))),
  CONSTRAINT `node_sessions_node_sessions_username_check` CHECK (((char_length(`username`) >= 1) and (char_length(`username`) <= 256))),
  CONSTRAINT `node_sessions_observed_at_range` CHECK (((`observed_at` is null) or (`observed_at` in (-(9223372036854775808),9223372036854775807)) or ((`observed_at` >= -(211813488000000000)) and (`observed_at` < 9223371331200000000)))),
  CONSTRAINT `node_sessions_text_no_nul` CHECK (((locate(0x00,cast(`session_id` as char charset binary)) = 0) and (locate(0x00,cast(`username` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `node_trust_convergence` (
  `node_id` varbinary(16) NOT NULL,
  `endpoint_id` longblob NOT NULL,
  `desired_state` longtext NOT NULL,
  `revision` bigint NOT NULL,
  `reason` longtext NOT NULL,
  `update_applied` tinyint(1) NOT NULL DEFAULT '0',
  `close_required` tinyint(1) NOT NULL,
  `close_applied` tinyint(1) NOT NULL DEFAULT '0',
  `locked_by` varbinary(16) DEFAULT NULL,
  `attempts` int NOT NULL DEFAULT '0',
  `last_error` longtext,
  `available_at` bigint NOT NULL,
  `locked_until` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`),
  KEY `node_trust_convergence_pending_idx` (`available_at`,`node_id`),
  CONSTRAINT `node_trust_convergence_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `node_trust_convergence_available_at_range` CHECK (((`available_at` is null) or (`available_at` in (-(9223372036854775808),9223372036854775807)) or ((`available_at` >= -(211813488000000000)) and (`available_at` < 9223371331200000000)))),
  CONSTRAINT `node_trust_convergence_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `node_trust_convergence_chk_2` CHECK ((`update_applied` in (0,1))),
  CONSTRAINT `node_trust_convergence_chk_3` CHECK ((`close_required` in (0,1))),
  CONSTRAINT `node_trust_convergence_chk_4` CHECK ((`close_applied` in (0,1))),
  CONSTRAINT `node_trust_convergence_chk_5` CHECK ((length(`locked_by`) = 16)),
  CONSTRAINT `node_trust_convergence_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `node_trust_convergence_locked_until_range` CHECK (((`locked_until` is null) or (`locked_until` in (-(9223372036854775808),9223372036854775807)) or ((`locked_until` >= -(211813488000000000)) and (`locked_until` < 9223371331200000000)))),
  CONSTRAINT `node_trust_convergence_node_trust_convergence_attempts_check` CHECK ((`attempts` >= 0)),
  CONSTRAINT `node_trust_convergence_node_trust_convergence_check` CHECK (((`locked_by` is null) = (`locked_until` is null))),
  CONSTRAINT `node_trust_convergence_node_trust_convergence_check1` CHECK (((0 = `close_applied`) or (0 <> `close_required`))),
  CONSTRAINT `node_trust_convergence_node_trust_convergence_check2` CHECK (((`desired_state` = _utf8mb4'revoked') or (0 = `close_required`))),
  CONSTRAINT `node_trust_convergence_node_trust_convergence_desir_faebc0c1984c` CHECK ((`desired_state` in (_utf8mb4'active',_utf8mb4'revoked'))),
  CONSTRAINT `node_trust_convergence_node_trust_convergence_endpoint_id_check` CHECK ((length(`endpoint_id`) = 32)),
  CONSTRAINT `node_trust_convergence_node_trust_convergence_reason_check` CHECK (((char_length(`reason`) >= 1) and (char_length(`reason`) <= 1024))),
  CONSTRAINT `node_trust_convergence_node_trust_convergence_revision_check` CHECK ((`revision` > 0)),
  CONSTRAINT `node_trust_convergence_text_no_nul` CHECK (((locate(0x00,cast(`desired_state` as char charset binary)) = 0) and (locate(0x00,cast(`reason` as char charset binary)) = 0) and (locate(0x00,cast(`last_error` as char charset binary)) = 0))),
  CONSTRAINT `node_trust_convergence_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `operation_events` (
  `sequence` bigint NOT NULL AUTO_INCREMENT,
  `id` varbinary(16) NOT NULL,
  `operation_id` varbinary(16) NOT NULL,
  `state` longtext NOT NULL,
  `occurred_at` bigint NOT NULL,
  PRIMARY KEY (`sequence`),
  UNIQUE KEY `operation_events_id_key` (`id`),
  KEY `operation_events_operation_sequence_idx` (`operation_id`,`sequence`),
  CONSTRAINT `operation_events_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE CASCADE,
  CONSTRAINT `operation_events_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `operation_events_chk_2` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `operation_events_occurred_at_range` CHECK (((`occurred_at` is null) or (`occurred_at` in (-(9223372036854775808),9223372036854775807)) or ((`occurred_at` >= -(211813488000000000)) and (`occurred_at` < 9223371331200000000)))),
  CONSTRAINT `operation_events_operation_events_state_check` CHECK ((`state` in (_utf8mb4'queued',_utf8mb4'dispatched',_utf8mb4'accepted',_utf8mb4'running',_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'unknown',_utf8mb4'expired',_utf8mb4'rolled_back',_utf8mb4'superseded'))),
  CONSTRAINT `operation_events_text_no_nul` CHECK ((locate(0x00,cast(`state` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `outbox_events` (
  `id` varbinary(16) NOT NULL,
  `command_id` varbinary(16) NOT NULL,
  `event_type` longtext NOT NULL,
  `payload` longblob NOT NULL,
  `locked_by` varbinary(16) DEFAULT NULL,
  `attempts` int NOT NULL DEFAULT '0',
  `last_error` longtext,
  `available_at` bigint NOT NULL,
  `locked_until` bigint DEFAULT NULL,
  `published_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `outbox_events_command_id_key` (`command_id`),
  KEY `outbox_events_dispatch_idx` (`available_at`,`id`),
  CONSTRAINT `outbox_events_command_id_fkey` FOREIGN KEY (`command_id`) REFERENCES `commands` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `outbox_events_available_at_range` CHECK (((`available_at` is null) or (`available_at` in (-(9223372036854775808),9223372036854775807)) or ((`available_at` >= -(211813488000000000)) and (`available_at` < 9223371331200000000)))),
  CONSTRAINT `outbox_events_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `outbox_events_chk_2` CHECK ((length(`command_id`) = 16)),
  CONSTRAINT `outbox_events_chk_3` CHECK ((length(`locked_by`) = 16)),
  CONSTRAINT `outbox_events_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `outbox_events_locked_until_range` CHECK (((`locked_until` is null) or (`locked_until` in (-(9223372036854775808),9223372036854775807)) or ((`locked_until` >= -(211813488000000000)) and (`locked_until` < 9223371331200000000)))),
  CONSTRAINT `outbox_events_outbox_events_attempts_check` CHECK ((`attempts` >= 0)),
  CONSTRAINT `outbox_events_outbox_events_check` CHECK (((`locked_by` is null) = (`locked_until` is null))),
  CONSTRAINT `outbox_events_outbox_events_check1` CHECK (((`published_at` is null) or (`locked_by` is null))),
  CONSTRAINT `outbox_events_outbox_events_event_type_check` CHECK ((`event_type` = _utf8mb4'command.dispatch')),
  CONSTRAINT `outbox_events_outbox_events_payload_check` CHECK (((length(`payload`) >= 1) and (length(`payload`) <= 1048576))),
  CONSTRAINT `outbox_events_published_at_range` CHECK (((`published_at` is null) or (`published_at` in (-(9223372036854775808),9223372036854775807)) or ((`published_at` >= -(211813488000000000)) and (`published_at` < 9223371331200000000)))),
  CONSTRAINT `outbox_events_text_no_nul` CHECK (((locate(0x00,cast(`event_type` as char charset binary)) = 0) and (locate(0x00,cast(`last_error` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `agent_command_results` (
  `event_id` varbinary(16) NOT NULL,
  `command_id` varbinary(16) NOT NULL,
  `idempotency_key` varbinary(16) NOT NULL,
  `payload_sha256` longblob,
  `state` longtext NOT NULL,
  `result` longblob NOT NULL,
  `error_code` longtext,
  `replayed` tinyint(1) NOT NULL,
  `semantic_payload_hash_version` smallint NOT NULL DEFAULT '0',
  `receipt_verification_status` longtext NOT NULL DEFAULT (_utf8mb4'legacy'),
  `receipt_failure_reason` longtext,
  `privd_attestation_key_id` longtext,
  `effect_record_id` varbinary(512) DEFAULT NULL,
  `effect_sequence` bigint DEFAULT NULL,
  `receipt_sha256` longblob,
  `privileged_result_proof` longblob,
  `partial_d150ec30c744` tinyint GENERATED ALWAYS AS ((case when (`receipt_verification_status` = _utf8mb4'verified') then 1 else NULL end)) STORED,
  `accepted_at` bigint DEFAULT NULL,
  `completed_at` bigint NOT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`event_id`),
  KEY `agent_command_results_receipt_idx` (`command_id`,`effect_record_id`,`effect_sequence`),
  KEY `agent_command_results_command_created_idx` (`command_id`,`created_at`),
  CONSTRAINT `agent_command_results_command_id_fkey` FOREIGN KEY (`command_id`) REFERENCES `commands` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `agent_command_results_event_id_fkey` FOREIGN KEY (`event_id`) REFERENCES `transport_events` (`event_id`) ON DELETE RESTRICT,
  CONSTRAINT `agent_command_results_accepted_at_range` CHECK (((`accepted_at` is null) or (`accepted_at` in (-(9223372036854775808),9223372036854775807)) or ((`accepted_at` >= -(211813488000000000)) and (`accepted_at` < 9223371331200000000)))),
  CONSTRAINT `agent_command_results_agent_command_results_check` CHECK (((`accepted_at` is null) or (`accepted_at` <= `completed_at`))),
  CONSTRAINT `agent_command_results_agent_command_results_check1` CHECK ((((`state` = _utf8mb4'succeeded') and (`payload_sha256` is not null) and (`accepted_at` is not null) and (`error_code` is null)) or ((`state` in (_utf8mb4'failed',_utf8mb4'unknown')) and (`payload_sha256` is not null) and (`accepted_at` is not null) and (`error_code` is not null) and ((`state` <> _utf8mb4'unknown') or (length(`result`) = 0))) or ((`state` = _utf8mb4'rejected') and (`accepted_at` is null) and (`error_code` is not null) and (length(`result`) = 0)))),
  CONSTRAINT `agent_command_results_agent_command_results_error_code_check` CHECK (((`error_code` is null) or ((char_length(`error_code`) >= 1) and (char_length(`error_code`) <= 128)))),
  CONSTRAINT `agent_command_results_agent_command_results_payload_sha256_check` CHECK (((`payload_sha256` is null) or (length(`payload_sha256`) = 32))),
  CONSTRAINT `agent_command_results_agent_command_results_receipt_4b41a00eb7e3` CHECK ((`receipt_verification_status` in (_utf8mb4'legacy',_utf8mb4'not_required',_utf8mb4'verified',_utf8mb4'missing',_utf8mb4'invalid',_utf8mb4'unknown_key',_utf8mb4'revoked_key'))),
  CONSTRAINT `agent_command_results_agent_command_results_receipt_831affe53a91` CHECK (((`receipt_failure_reason` is null) or ((char_length(`receipt_failure_reason`) >= 1) and (char_length(`receipt_failure_reason`) <= 64)))),
  CONSTRAINT `agent_command_results_agent_command_results_receipt_fields_check` CHECK ((((`receipt_verification_status` = _utf8mb4'verified') and (`receipt_failure_reason` is null) and (`privd_attestation_key_id` is not null) and (`effect_record_id` is not null) and (length(`effect_record_id`) >= 16) and (length(`effect_record_id`) <= 32) and (`effect_sequence` > 0) and (length(`receipt_sha256`) = 32) and (length(`privileged_result_proof`) >= 1) and (length(`privileged_result_proof`) <= 65536)) or (`receipt_verification_status` <> _utf8mb4'verified'))),
  CONSTRAINT `agent_command_results_agent_command_results_result_check` CHECK ((length(`result`) <= 1048576)),
  CONSTRAINT `agent_command_results_agent_command_results_semanti_fc5f0c4bc2ff` CHECK ((`semantic_payload_hash_version` in (0,1,2))),
  CONSTRAINT `agent_command_results_agent_command_results_state_check` CHECK ((`state` in (_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'unknown',_utf8mb4'rejected'))),
  CONSTRAINT `agent_command_results_chk_1` CHECK ((length(`event_id`) = 16)),
  CONSTRAINT `agent_command_results_chk_2` CHECK ((length(`command_id`) = 16)),
  CONSTRAINT `agent_command_results_chk_3` CHECK ((length(`idempotency_key`) = 16)),
  CONSTRAINT `agent_command_results_chk_4` CHECK ((`replayed` in (0,1))),
  CONSTRAINT `agent_command_results_completed_at_range` CHECK (((`completed_at` is null) or (`completed_at` in (-(9223372036854775808),9223372036854775807)) or ((`completed_at` >= -(211813488000000000)) and (`completed_at` < 9223371331200000000)))),
  CONSTRAINT `agent_command_results_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `agent_command_results_text_no_nul` CHECK (((locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`error_code` as char charset binary)) = 0) and (locate(0x00,cast(`receipt_verification_status` as char charset binary)) = 0) and (locate(0x00,cast(`receipt_failure_reason` as char charset binary)) = 0) and (locate(0x00,cast(`privd_attestation_key_id` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `agent_rollout_nodes` (
  `rollout_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `ordinal` int NOT NULL,
  `batch` int NOT NULL,
  `state` longtext NOT NULL DEFAULT (_utf8mb4'pending'),
  `operation_id` varbinary(16) DEFAULT NULL,
  `from_version` longtext NOT NULL DEFAULT (_utf8mb4''),
  `failure_code` longtext NOT NULL DEFAULT (_utf8mb4''),
  `dispatch_node_version` bigint DEFAULT NULL,
  `dispatch_attempt` int NOT NULL DEFAULT '0',
  `dispatch_lease_until` bigint DEFAULT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`rollout_id`,`node_id`),
  UNIQUE KEY `agent_rollout_nodes_rollout_id_ordinal_key` (`rollout_id`,`ordinal`),
  KEY `agent_rollout_nodes_operation_idx` (`operation_id`),
  KEY `agent_rollout_nodes_active_idx` (`rollout_id`,`batch`),
  CONSTRAINT `agent_rollout_nodes_rollout_id_fkey` FOREIGN KEY (`rollout_id`) REFERENCES `agent_rollouts` (`id`) ON DELETE CASCADE,
  CONSTRAINT `agent_rollout_nodes_agent_rollout_nodes_batch_check` CHECK ((`batch` >= 0)),
  CONSTRAINT `agent_rollout_nodes_agent_rollout_nodes_dispatch_attempt_check` CHECK ((`dispatch_attempt` >= 0)),
  CONSTRAINT `agent_rollout_nodes_agent_rollout_nodes_failure_code_check` CHECK ((char_length(`failure_code`) <= 128)),
  CONSTRAINT `agent_rollout_nodes_agent_rollout_nodes_ordinal_check` CHECK ((`ordinal` >= 0)),
  CONSTRAINT `agent_rollout_nodes_agent_rollout_nodes_state_check` CHECK ((`state` in (_utf8mb4'pending',_utf8mb4'running',_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'rolled_back',_utf8mb4'unknown',_utf8mb4'skipped'))),
  CONSTRAINT `agent_rollout_nodes_chk_1` CHECK ((length(`rollout_id`) = 16)),
  CONSTRAINT `agent_rollout_nodes_chk_2` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `agent_rollout_nodes_chk_3` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `agent_rollout_nodes_dispatch_lease_until_range` CHECK (((`dispatch_lease_until` is null) or (`dispatch_lease_until` in (-(9223372036854775808),9223372036854775807)) or ((`dispatch_lease_until` >= -(211813488000000000)) and (`dispatch_lease_until` < 9223371331200000000)))),
  CONSTRAINT `agent_rollout_nodes_text_no_nul` CHECK (((locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`from_version` as char charset binary)) = 0) and (locate(0x00,cast(`failure_code` as char charset binary)) = 0))),
  CONSTRAINT `agent_rollout_nodes_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `audit_checkpoints` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `through_event_id` varbinary(16) NOT NULL,
  `through_event_hash` longblob NOT NULL,
  `signature` longblob NOT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `audit_checkpoints_workspace_id_through_event_id_key` (`workspace_id`,`through_event_id`),
  KEY `audit_checkpoints_through_event_id_fkey` (`through_event_id`),
  CONSTRAINT `audit_checkpoints_through_event_id_fkey` FOREIGN KEY (`through_event_id`) REFERENCES `audit_events` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `audit_checkpoints_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `audit_checkpoints_audit_checkpoints_signature_check` CHECK ((length(`signature`) = 32)),
  CONSTRAINT `audit_checkpoints_audit_checkpoints_through_event_hash_check` CHECK ((length(`through_event_hash`) = 32)),
  CONSTRAINT `audit_checkpoints_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `audit_checkpoints_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `audit_checkpoints_chk_3` CHECK ((length(`through_event_id`) = 16)),
  CONSTRAINT `audit_checkpoints_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `certificates` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `operation_id` varbinary(16) NOT NULL,
  `common_name` longtext NOT NULL,
  `key_bits` int NOT NULL,
  `state` longtext NOT NULL,
  `csr_der` longblob,
  `public_key_sha256` longblob,
  `certificate_chain_pem` longblob,
  `serial_number` longtext,
  `revocation_reason` longtext,
  `issue_approval_id` varbinary(16) DEFAULT NULL,
  `issue_request_hash` longblob,
  `issue_actor_identity_id` varbinary(16) DEFAULT NULL,
  `version` bigint NOT NULL DEFAULT '1',
  `csr_receipt_legacy` tinyint(1) NOT NULL DEFAULT '0',
  `csr_receipt_sha256` longblob,
  `csr_privd_attestation_key_id` varchar(191) DEFAULT NULL,
  `csr_effect_record_id` longblob,
  `csr_der_sha256` longblob,
  `csr_requested_subject_sha256` longblob,
  `issue_certificate_version` bigint DEFAULT NULL,
  `not_after` bigint DEFAULT NULL,
  `not_before` bigint DEFAULT NULL,
  `revoked_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `csr_receipt_verified_at` bigint DEFAULT NULL,
  `dns_names` longblob NOT NULL DEFAULT (_utf8mb4'[]'),
  PRIMARY KEY (`id`),
  UNIQUE KEY `certificates_operation_id_key` (`operation_id`),
  KEY `certificates_csr_privd_key_fk` (`node_id`,`csr_privd_attestation_key_id`),
  KEY `certificates_issue_actor_identity_id_fkey` (`issue_actor_identity_id`),
  KEY `certificates_issue_approval_id_fkey` (`issue_approval_id`),
  KEY `certificates_workspace_id_node_id_fkey` (`workspace_id`,`node_id`),
  KEY `certificates_node_expiry_idx` (`node_id`,`not_after`),
  CONSTRAINT `certificates_csr_privd_key_fk` FOREIGN KEY (`node_id`, `csr_privd_attestation_key_id`) REFERENCES `node_privd_attestation_keys` (`node_id`, `key_id`),
  CONSTRAINT `certificates_issue_actor_identity_id_fkey` FOREIGN KEY (`issue_actor_identity_id`) REFERENCES `identities` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `certificates_issue_approval_id_fkey` FOREIGN KEY (`issue_approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `certificates_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `certificates_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `certificates_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `certificates_workspace_id_node_id_fkey` FOREIGN KEY (`workspace_id`, `node_id`) REFERENCES `nodes` (`workspace_id`, `id`) ON DELETE RESTRICT,
  CONSTRAINT `certificates_certificates_certificate_chain_pem_check` CHECK (((`certificate_chain_pem` is null) or ((length(`certificate_chain_pem`) >= 64) and (length(`certificate_chain_pem`) <= 262144)))),
  CONSTRAINT `certificates_certificates_common_name_check` CHECK (((char_length(`common_name`) >= 1) and (char_length(`common_name`) <= 253))),
  CONSTRAINT `certificates_certificates_csr_der_check` CHECK (((`csr_der` is null) or ((length(`csr_der`) >= 64) and (length(`csr_der`) <= 65536)))),
  CONSTRAINT `certificates_certificates_csr_receipt_check` CHECK ((((`state` in (_utf8mb4'csr_ready',_utf8mb4'signing',_utf8mb4'signer_unavailable',_utf8mb4'issued',_utf8mb4'expiring',_utf8mb4'expired',_utf8mb4'revoking',_utf8mb4'revocation_unknown',_utf8mb4'revoked')) and (`csr_receipt_verified_at` is not null) and (length(`csr_receipt_sha256`) = 32) and (`csr_privd_attestation_key_id` is not null) and (length(`csr_effect_record_id`) between 16 and 32) and (length(`csr_der_sha256`) = 32) and (length(`csr_requested_subject_sha256`) = 32)) or (0 <> `csr_receipt_legacy`) or (`state` in (_utf8mb4'csr_pending',_utf8mb4'failed',_utf8mb4'unknown')))),
  CONSTRAINT `certificates_certificates_issue_certificate_version_check` CHECK ((`issue_certificate_version` > 0)),
  CONSTRAINT `certificates_certificates_issue_request_hash_check` CHECK (((`issue_request_hash` is null) or (length(`issue_request_hash`) = 32))),
  CONSTRAINT `certificates_certificates_key_bits_check` CHECK ((`key_bits` in (2048,3072,4096))),
  CONSTRAINT `certificates_certificates_public_key_sha256_check` CHECK (((`public_key_sha256` is null) or (length(`public_key_sha256`) = 32))),
  CONSTRAINT `certificates_certificates_revocation_reason_check` CHECK (((`revocation_reason` is null) or ((char_length(`revocation_reason`) >= 1) and (char_length(`revocation_reason`) <= 128)))),
  CONSTRAINT `certificates_certificates_serial_number_check` CHECK (((`serial_number` is null) or ((char_length(`serial_number`) >= 1) and (char_length(`serial_number`) <= 128)))),
  CONSTRAINT `certificates_certificates_state_check` CHECK ((`state` in (_utf8mb4'csr_pending',_utf8mb4'csr_ready',_utf8mb4'signing',_utf8mb4'signer_unavailable',_utf8mb4'issued',_utf8mb4'expiring',_utf8mb4'expired',_utf8mb4'revoking',_utf8mb4'revocation_unknown',_utf8mb4'revoked',_utf8mb4'failed',_utf8mb4'unknown'))),
  CONSTRAINT `certificates_certificates_version_check` CHECK ((`version` > 0)),
  CONSTRAINT `certificates_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `certificates_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `certificates_chk_3` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `certificates_chk_4` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `certificates_chk_5` CHECK ((length(`issue_approval_id`) = 16)),
  CONSTRAINT `certificates_chk_6` CHECK ((length(`issue_actor_identity_id`) = 16)),
  CONSTRAINT `certificates_chk_7` CHECK ((`csr_receipt_legacy` in (0,1))),
  CONSTRAINT `certificates_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -211813488000000000) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `certificates_csr_receipt_verified_at_range` CHECK (((`csr_receipt_verified_at` is null) or (`csr_receipt_verified_at` in (-(9223372036854775808),9223372036854775807)) or ((`csr_receipt_verified_at` >= -211813488000000000) and (`csr_receipt_verified_at` < 9223371331200000000)))),
  CONSTRAINT `certificates_not_after_range` CHECK (((`not_after` is null) or (`not_after` in (-(9223372036854775808),9223372036854775807)) or ((`not_after` >= -211813488000000000) and (`not_after` < 9223371331200000000)))),
  CONSTRAINT `certificates_not_before_range` CHECK (((`not_before` is null) or (`not_before` in (-(9223372036854775808),9223372036854775807)) or ((`not_before` >= -211813488000000000) and (`not_before` < 9223371331200000000)))),
  CONSTRAINT `certificates_revoked_at_range` CHECK (((`revoked_at` is null) or (`revoked_at` in (-(9223372036854775808),9223372036854775807)) or ((`revoked_at` >= -211813488000000000) and (`revoked_at` < 9223371331200000000)))),
  CONSTRAINT `certificates_text_no_nul` CHECK (((locate(0x00,cast(`common_name` as char charset binary)) = 0) and (locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`serial_number` as char charset binary)) = 0) and (locate(0x00,cast(`revocation_reason` as char charset binary)) = 0) and (locate(0x00,cast(`csr_privd_attestation_key_id` as char charset binary)) = 0))),
  CONSTRAINT `certificates_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -211813488000000000) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `command_attempts` (
  `id` varbinary(16) NOT NULL,
  `command_id` varbinary(16) NOT NULL,
  `outbox_event_id` varbinary(16) NOT NULL,
  `worker_id` varbinary(16) NOT NULL,
  `attempt_number` int NOT NULL,
  `state` longtext NOT NULL,
  `error_code` longtext,
  `started_at` bigint NOT NULL,
  `finished_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `command_attempts_command_id_attempt_number_key` (`command_id`,`attempt_number`),
  KEY `command_attempts_outbox_event_id_fkey` (`outbox_event_id`),
  CONSTRAINT `command_attempts_command_id_fkey` FOREIGN KEY (`command_id`) REFERENCES `commands` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `command_attempts_outbox_event_id_fkey` FOREIGN KEY (`outbox_event_id`) REFERENCES `outbox_events` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `command_attempts_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `command_attempts_chk_2` CHECK ((length(`command_id`) = 16)),
  CONSTRAINT `command_attempts_chk_3` CHECK ((length(`outbox_event_id`) = 16)),
  CONSTRAINT `command_attempts_chk_4` CHECK ((length(`worker_id`) = 16)),
  CONSTRAINT `command_attempts_command_attempts_attempt_number_check` CHECK ((`attempt_number` > 0)),
  CONSTRAINT `command_attempts_command_attempts_check` CHECK (((`state` = _utf8mb4'sending') = (`finished_at` is null))),
  CONSTRAINT `command_attempts_command_attempts_state_check` CHECK ((`state` in (_utf8mb4'sending',_utf8mb4'sent',_utf8mb4'failed',_utf8mb4'unknown'))),
  CONSTRAINT `command_attempts_finished_at_range` CHECK (((`finished_at` is null) or (`finished_at` in (-(9223372036854775808),9223372036854775807)) or ((`finished_at` >= -(211813488000000000)) and (`finished_at` < 9223371331200000000)))),
  CONSTRAINT `command_attempts_started_at_range` CHECK (((`started_at` is null) or (`started_at` in (-(9223372036854775808),9223372036854775807)) or ((`started_at` >= -(211813488000000000)) and (`started_at` < 9223371331200000000)))),
  CONSTRAINT `command_attempts_text_no_nul` CHECK (((locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`error_code` as char charset binary)) = 0)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `config_apply_operations` (
  `operation_id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `plan_id` varbinary(16) NOT NULL,
  `approval_id` varbinary(16) NOT NULL,
  `expected_revision` bigint NOT NULL,
  `desired_revision` bigint NOT NULL,
  `candidate_hash` longblob NOT NULL,
  `previous_hash` longblob NOT NULL,
  `state` longtext NOT NULL,
  `failure_code` longtext,
  `partial_2c23369cc864` tinyint GENERATED ALWAYS AS ((case when (`state` in (_utf8mb4'queued',_utf8mb4'dispatched',_utf8mb4'accepted',_utf8mb4'running',_utf8mb4'unknown')) then 1 else NULL end)) STORED,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`operation_id`),
  UNIQUE KEY `config_apply_operations_plan_id_key` (`plan_id`),
  UNIQUE KEY `config_apply_operations_one_active_node_idx` (`node_id`,`partial_2c23369cc864`),
  KEY `config_apply_operations_approval_id_fkey` (`approval_id`),
  KEY `config_apply_operations_workspace_id_node_id_fkey` (`workspace_id`,`node_id`),
  KEY `config_apply_operations_node_created_idx` (`node_id`,`created_at` DESC,`operation_id` DESC),
  CONSTRAINT `config_apply_operations_approval_id_fkey` FOREIGN KEY (`approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_apply_operations_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_apply_operations_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_apply_operations_plan_id_fkey` FOREIGN KEY (`plan_id`) REFERENCES `config_plans` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_apply_operations_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `config_apply_operations_workspace_id_node_id_fkey` FOREIGN KEY (`workspace_id`, `node_id`) REFERENCES `nodes` (`workspace_id`, `id`) ON DELETE RESTRICT,
  CONSTRAINT `config_apply_operations_chk_1` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `config_apply_operations_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `config_apply_operations_chk_3` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `config_apply_operations_chk_4` CHECK ((length(`plan_id`) = 16)),
  CONSTRAINT `config_apply_operations_chk_5` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `config_apply_operations_config_apply_operations_can_9fdd2cab0627` CHECK ((length(`candidate_hash`) = 32)),
  CONSTRAINT `config_apply_operations_config_apply_operations_des_d0157fb52cf7` CHECK (((`expected_revision` >= 0) and (`desired_revision` > `expected_revision`))),
  CONSTRAINT `config_apply_operations_config_apply_operations_exp_c494cf0c238a` CHECK ((`expected_revision` >= 0)),
  CONSTRAINT `config_apply_operations_config_apply_operations_fai_1d828f15deb0` CHECK (((`failure_code` is null) or ((char_length(`failure_code`) >= 1) and (char_length(`failure_code`) <= 128)))),
  CONSTRAINT `config_apply_operations_config_apply_operations_pre_a038d201542b` CHECK ((length(`previous_hash`) = 32)),
  CONSTRAINT `config_apply_operations_config_apply_operations_state_check` CHECK ((`state` in (_utf8mb4'queued',_utf8mb4'dispatched',_utf8mb4'accepted',_utf8mb4'running',_utf8mb4'succeeded',_utf8mb4'failed',_utf8mb4'rolled_back',_utf8mb4'failed_critical',_utf8mb4'unknown',_utf8mb4'expired'))),
  CONSTRAINT `config_apply_operations_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `config_apply_operations_text_no_nul` CHECK (((locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`failure_code` as char charset binary)) = 0))),
  CONSTRAINT `config_apply_operations_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `desired_user_policies` (
  `node_id` varbinary(16) NOT NULL,
  `username` varchar(191) NOT NULL,
  `quota_period` longtext NOT NULL,
  `quota_direction` longtext NOT NULL,
  `quota_bytes` bigint NOT NULL,
  `version` bigint NOT NULL,
  `expires_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  PRIMARY KEY (`node_id`,`username`),
  CONSTRAINT `desired_user_policies_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `desired_user_policies_node_id_username_fkey` FOREIGN KEY (`node_id`, `username`) REFERENCES `desired_users` (`node_id`, `username`) ON DELETE CASCADE,
  CONSTRAINT `desired_user_policies_chk_1` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `desired_user_policies_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `desired_user_policies_desired_user_policies_check` CHECK (((`quota_period` = _utf8mb4'none') = (`quota_bytes` = 0))),
  CONSTRAINT `desired_user_policies_desired_user_policies_quota_bytes_check` CHECK (((`quota_bytes` >= 0) and (`quota_bytes` <= 9007199254740991))),
  CONSTRAINT `desired_user_policies_desired_user_policies_quota_d_8c9405445eec` CHECK ((`quota_direction` in (_utf8mb4'rx',_utf8mb4'tx',_utf8mb4'rxtx'))),
  CONSTRAINT `desired_user_policies_desired_user_policies_quota_period_check` CHECK ((`quota_period` in (_utf8mb4'none',_utf8mb4'monthly',_utf8mb4'lifetime'))),
  CONSTRAINT `desired_user_policies_desired_user_policies_username_check` CHECK (regexp_like(`username`,_utf8mb4'(?-i)\\A[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\\z')),
  CONSTRAINT `desired_user_policies_desired_user_policies_version_check` CHECK ((`version` > 0)),
  CONSTRAINT `desired_user_policies_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -(211813488000000000)) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `desired_user_policies_text_no_nul` CHECK (((locate(0x00,cast(`username` as char charset binary)) = 0) and (locate(0x00,cast(`quota_period` as char charset binary)) = 0) and (locate(0x00,cast(`quota_direction` as char charset binary)) = 0))),
  CONSTRAINT `desired_user_policies_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `exact_agent_command_results` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_agent_command_results_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `agent_command_results` (`event_id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE `user_policy_mutations` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `username` varchar(191) NOT NULL,
  `idempotency_key` varchar(191) NOT NULL,
  `request_hash` longblob NOT NULL,
  `policy_version` bigint NOT NULL,
  `created_at` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `user_policy_mutations_workspace_id_idempotency_key_key` (`workspace_id`,`idempotency_key`),
  KEY `user_policy_mutations_node_id_username_fkey` (`node_id`,`username`),
  CONSTRAINT `user_policy_mutations_node_id_username_fkey` FOREIGN KEY (`node_id`, `username`) REFERENCES `desired_user_policies` (`node_id`, `username`) ON DELETE RESTRICT,
  CONSTRAINT `user_policy_mutations_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `user_policy_mutations_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `user_policy_mutations_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `user_policy_mutations_chk_3` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `user_policy_mutations_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `user_policy_mutations_text_no_nul` CHECK (((locate(0x00,cast(`username` as char charset binary)) = 0) and (locate(0x00,cast(`idempotency_key` as char charset binary)) = 0))),
  CONSTRAINT `user_policy_mutations_user_policy_mutations_idempot_750a7e26bf21` CHECK (((char_length(`idempotency_key`) >= 1) and (char_length(`idempotency_key`) <= 128))),
  CONSTRAINT `user_policy_mutations_user_policy_mutations_policy_version_check` CHECK ((`policy_version` > 0)),
  CONSTRAINT `user_policy_mutations_user_policy_mutations_request_hash_check` CHECK ((length(`request_hash`) = 32))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE `artifact_operations` (
  `id` varbinary(16) NOT NULL,
  `workspace_id` varbinary(16) NOT NULL,
  `node_id` varbinary(16) NOT NULL,
  `certificate_id` varbinary(16) NOT NULL,
  `operation_id` varbinary(16) NOT NULL,
  `purpose` longtext NOT NULL,
  `state` longtext NOT NULL,
  `content_sha256` longblob,
  `content_size` bigint DEFAULT NULL,
  `token_sha256` longblob NOT NULL,
  `request_hash` longblob NOT NULL,
  `approval_id` varbinary(16) DEFAULT NULL,
  `certificate_version` bigint NOT NULL,
  `active_grant_id` varbinary(16) DEFAULT NULL,
  `active_grant_subject` longtext,
  `consume_grant` longblob,
  `consume_sha256` longblob,
  `consume_size` bigint DEFAULT NULL,
  `consume_actor_id` varbinary(16) DEFAULT NULL,
  `consume_session_id` varbinary(16) DEFAULT NULL,
  `consume_request_id` longtext,
  `partial_9e31abe69a6b` tinyint GENERATED ALWAYS AS ((case when (`active_grant_id` is not null) then 1 else NULL end)) STORED,
  `partial_8d99c0b136e1` tinyint GENERATED ALWAYS AS ((case when (`state` in (_utf8mb4'pending',_utf8mb4'ready',_utf8mb4'leased',_utf8mb4'consuming')) then 1 else NULL end)) STORED,
  `expires_at` bigint NOT NULL,
  `lease_until` bigint DEFAULT NULL,
  `consumed_at` bigint DEFAULT NULL,
  `created_at` bigint NOT NULL,
  `updated_at` bigint NOT NULL,
  `active_grant_expires_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `artifact_operations_operation_id_key` (`operation_id`),
  UNIQUE KEY `artifact_operations_active_grant_idx` (`active_grant_id`,`partial_9e31abe69a6b`),
  UNIQUE KEY `artifact_operations_one_live_certificate_idx` (`certificate_id`,`partial_8d99c0b136e1`),
  KEY `artifact_operations_approval_id_fkey` (`approval_id`),
  KEY `artifact_operations_node_id_fkey` (`node_id`),
  KEY `artifact_operations_workspace_id_fkey` (`workspace_id`),
  KEY `artifact_operations_expiry_idx` (`expires_at`),
  CONSTRAINT `artifact_operations_approval_id_fkey` FOREIGN KEY (`approval_id`) REFERENCES `approval_requests` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `artifact_operations_certificate_id_fkey` FOREIGN KEY (`certificate_id`) REFERENCES `certificates` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `artifact_operations_node_id_fkey` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `artifact_operations_operation_id_fkey` FOREIGN KEY (`operation_id`) REFERENCES `operations` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `artifact_operations_workspace_id_fkey` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE RESTRICT,
  CONSTRAINT `artifact_operations_active_grant_expires_at_range` CHECK (((`active_grant_expires_at` is null) or (`active_grant_expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`active_grant_expires_at` >= -(211813488000000000)) and (`active_grant_expires_at` < 9223371331200000000)))),
  CONSTRAINT `artifact_operations_artifact_operations_certificate_03a995139fe1` CHECK ((`certificate_version` > 0)),
  CONSTRAINT `artifact_operations_artifact_operations_check` CHECK ((((`state` = _utf8mb4'ready') and (`content_sha256` is not null) and (`content_size` is not null)) or (`state` <> _utf8mb4'ready'))),
  CONSTRAINT `artifact_operations_artifact_operations_consume_check` CHECK ((((`state` = _utf8mb4'consuming') and (`consume_grant` is not null) and (length(`consume_grant`) >= 1) and (length(`consume_grant`) <= 4096) and (`consume_sha256` is not null) and (length(`consume_sha256`) = 32) and (`consume_size` > 0) and (`consume_actor_id` is not null) and (`consume_session_id` is not null) and (`consume_request_id` is not null) and (char_length(`consume_request_id`) >= 1) and (char_length(`consume_request_id`) <= 128)) or (`state` <> _utf8mb4'consuming'))),
  CONSTRAINT `artifact_operations_artifact_operations_content_sha256_check` CHECK (((`content_sha256` is null) or (length(`content_sha256`) = 32))),
  CONSTRAINT `artifact_operations_artifact_operations_content_size_check` CHECK (((`content_size` is null) or ((`content_size` >= 1) and (`content_size` <= 67108864)))),
  CONSTRAINT `artifact_operations_artifact_operations_grant_check` CHECK ((((`state` = _utf8mb4'leased') and (`active_grant_id` is not null) and (`active_grant_subject` is not null) and (`active_grant_expires_at` is not null)) or (`state` <> _utf8mb4'leased'))),
  CONSTRAINT `artifact_operations_artifact_operations_purpose_check` CHECK ((`purpose` = _utf8mb4'certificate_p12')),
  CONSTRAINT `artifact_operations_artifact_operations_request_hash_check` CHECK ((length(`request_hash`) = 32)),
  CONSTRAINT `artifact_operations_artifact_operations_state_check` CHECK ((`state` in (_utf8mb4'pending',_utf8mb4'ready',_utf8mb4'leased',_utf8mb4'consuming',_utf8mb4'consumed',_utf8mb4'expired',_utf8mb4'revoked',_utf8mb4'failed'))),
  CONSTRAINT `artifact_operations_artifact_operations_token_sha256_check` CHECK ((length(`token_sha256`) = 32)),
  CONSTRAINT `artifact_operations_chk_1` CHECK ((length(`id`) = 16)),
  CONSTRAINT `artifact_operations_chk_2` CHECK ((length(`workspace_id`) = 16)),
  CONSTRAINT `artifact_operations_chk_3` CHECK ((length(`node_id`) = 16)),
  CONSTRAINT `artifact_operations_chk_4` CHECK ((length(`certificate_id`) = 16)),
  CONSTRAINT `artifact_operations_chk_5` CHECK ((length(`operation_id`) = 16)),
  CONSTRAINT `artifact_operations_chk_6` CHECK ((length(`approval_id`) = 16)),
  CONSTRAINT `artifact_operations_chk_7` CHECK ((length(`active_grant_id`) = 16)),
  CONSTRAINT `artifact_operations_chk_8` CHECK ((length(`consume_actor_id`) = 16)),
  CONSTRAINT `artifact_operations_chk_9` CHECK ((length(`consume_session_id`) = 16)),
  CONSTRAINT `artifact_operations_consumed_at_range` CHECK (((`consumed_at` is null) or (`consumed_at` in (-(9223372036854775808),9223372036854775807)) or ((`consumed_at` >= -(211813488000000000)) and (`consumed_at` < 9223371331200000000)))),
  CONSTRAINT `artifact_operations_created_at_range` CHECK (((`created_at` is null) or (`created_at` in (-(9223372036854775808),9223372036854775807)) or ((`created_at` >= -(211813488000000000)) and (`created_at` < 9223371331200000000)))),
  CONSTRAINT `artifact_operations_expires_at_range` CHECK (((`expires_at` is null) or (`expires_at` in (-(9223372036854775808),9223372036854775807)) or ((`expires_at` >= -(211813488000000000)) and (`expires_at` < 9223371331200000000)))),
  CONSTRAINT `artifact_operations_lease_until_range` CHECK (((`lease_until` is null) or (`lease_until` in (-(9223372036854775808),9223372036854775807)) or ((`lease_until` >= -(211813488000000000)) and (`lease_until` < 9223371331200000000)))),
  CONSTRAINT `artifact_operations_text_no_nul` CHECK (((locate(0x00,cast(`purpose` as char charset binary)) = 0) and (locate(0x00,cast(`state` as char charset binary)) = 0) and (locate(0x00,cast(`active_grant_subject` as char charset binary)) = 0) and (locate(0x00,cast(`consume_request_id` as char charset binary)) = 0))),
  CONSTRAINT `artifact_operations_updated_at_range` CHECK (((`updated_at` is null) or (`updated_at` in (-(9223372036854775808),9223372036854775807)) or ((`updated_at` >= -(211813488000000000)) and (`updated_at` < 9223371331200000000))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
INSERT INTO `business_locks` (`lock_key`) VALUES (CAST(X'74656c656d657472792d73686172642d636174616c6f67' AS BINARY));
INSERT INTO `exact_key_guards` (`key_name`) VALUES (CAST(X'6167656e745f636f6d6d616e645f726573756c7473' AS BINARY)),(CAST(X'6964656e746974696573' AS BINARY)),(CAST(X'6e6f646573' AS BINARY)),(CAST(X'6f7065726174696f6e73' AS BINARY)),(CAST(X'74656c656d657472795f726f6c6c7570735f3168' AS BINARY)),(CAST(X'74656c656d657472795f726f6c6c7570735f356d' AS BINARY)),(CAST(X'757073747265616d5f73796e635f7265636f726473' AS BINARY)),(CAST(X'757365725f706f6c6963795f656e666f7263656d656e7473' AS BINARY)),(CAST(X'776f726b737061636573' AS BINARY));
INSERT INTO `roles` (`name`) VALUES (CAST(X'41756469746f72' AS BINARY)),(CAST(X'436f6e6669674d616e61676572' AS BINARY)),(CAST(X'4f70657261746f72' AS BINARY)),(CAST(X'506c6174666f726d41646d696e' AS BINARY)),(CAST(X'536563757269747941646d696e' AS BINARY)),(CAST(X'557365724d616e61676572' AS BINARY)),(CAST(X'566965776572' AS BINARY));
INSERT INTO `scheduler_leadership` (`id`,`instance_id`,`incarnation`,`epoch`,`lease_until`,`updated_at`) VALUES (CAST(X'31' AS BINARY),CAST(X'00000000000000000000000000000000' AS BINARY),CAST(X'30' AS BINARY),CAST(X'30' AS BINARY),CAST(X'2d39323233333732303336383534373735383038' AS BINARY),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)));
INSERT INTO `telemetry_legacy_migration` (`singleton`,`state`,`completed_at`) VALUES (CAST(X'31' AS BINARY),CAST(X'70656e64696e67' AS BINARY),NULL);
INSERT INTO `telemetry_maintenance_progress` (`singleton`,`phase`,`cutoff`,`candidate`,`cursor_at`,`cursor_node`,`cursor_metric`) VALUES (CAST(X'31' AS BINARY),CAST(X'30' AS BINARY),CAST(X'30' AS BINARY),NULL,NULL,NULL,NULL);
INSERT INTO `upstream_sync_records` (`id`,`repository`,`old_ref`,`old_commit`,`new_ref`,`new_commit`,`rollback_ref`,`synced_at`,`classification`) VALUES (CAST(X'019fdc5bb93972a1ae678efd197e5688' AS BINARY),CAST(X'6d6d746165652f6f63736572762d64617368626f617264' AS BINARY),CAST(X'76342e39' AS BINARY),CAST(X'62386635393032366334643837396634306331646134336463303064393765333466393739306263' AS BINARY),CAST(X'6d6173746572' AS BINARY),CAST(X'34643235343738353830643839396237373436306264663063663061353930636664643236303330' AS BINARY),CAST(X'7075626c69636174696f6e3a20726576657274205052313520696e646570656e64656e746c793b20696d706c656d656e746174696f6e3a2073746f7020493134207363686564756c65722f4150492c207265636f6e63696c6520636f6d6d616e64732c2072657665727420505231342c207468656e206170706c79206d6967726174696f6e2030303030313320646f776e206f6e6c79207768656e20706f6c69637920616e642062617463682064617461206e656564206e6f742062652072657461696e6564' AS BINARY),CAST(X'383339343336313132303030303030' AS BINARY),CAST(X'7b2241223a205b5d2c202242223a205b227765622f7372632f636f6d706f6e656e74732f617574682f5365747570466f726d2e767565225d2c202243223a205b2271756f746120616e64206578706972792073656d616e74696373206d617070656420746f206e6f64652d73636f706564206465736972656420706f6c69637920616e64207363686564756c6572225d2c202244223a205b22446f636b65722f6e6174697665206f6363746c20657865637574696f6e222c20226c6f63616c2063726f6e206a6f75726e616c222c20226469726563742070617373776f72642f636f6e6669672066696c6573222c20227065726d616e656e742064656c6574696f6e225d7d' AS BINARY));
INSERT INTO `exact_upstream_sync_records` (`owner_id`,`key_value`) VALUES (CAST(X'019fdc5bb93972a1ae678efd197e5688' AS BINARY),CAST(X'00000000000000176d6d746165652f6f63736572762d64617368626f617264000000000000002862386635393032366334643837396634306331646134336463303064393765333466393739306263000000000000002834643235343738353830643839396237373436306264663063663061353930636664643236303330' AS BINARY));
CREATE FUNCTION `ocserv_jsonb_array_valid`(doc LONGBLOB) RETURNS tinyint(1)
    NO SQL
    DETERMINISTIC
BEGIN
 DECLARE source LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE masked LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT '';
 DECLARE token LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE digits LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE c CHAR(1) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE posn BIGINT DEFAULT 1;
 DECLARE startn BIGINT;
 DECLARE n BIGINT;
 DECLARE dotn BIGINT;
 DECLARE en BIGINT;
 DECLARE expo BIGINT;
 DECLARE scale_n BIGINT;
 DECLARE integer_n BIGINT;
 DECLARE leading_n BIGINT;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION RETURN FALSE;
 IF doc IS NULL THEN RETURN FALSE; END IF;
 SET source=CONVERT(doc USING utf8mb4);
 IF CAST(source AS BINARY)<>doc THEN RETURN FALSE; END IF;
 SET n=CHAR_LENGTH(source);
 WHILE posn<=n DO
  SET c=SUBSTRING(source,posn,1);
  IF c='"' THEN
   SET startn=posn;
   SET posn=posn+1;
   WHILE posn<=n AND SUBSTRING(source,posn,1)<>'"' DO
    IF ASCII(SUBSTRING(source,posn,1))=92 THEN SET posn=posn+1; END IF;
    SET posn=posn+1;
   END WHILE;
   IF posn>n THEN RETURN FALSE; END IF;
   SET token=SUBSTRING(source,startn,posn-startn+1);
   IF NOT JSON_VALID(token) OR LOCATE(0x00,CAST(JSON_UNQUOTE(token) AS BINARY))<>0 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,token);
   SET posn=posn+1;
  ELSEIF c='-' OR (ASCII(c)>=48 AND ASCII(c)<=57) THEN
   SET startn=posn;
   WHILE posn<=n AND LOCATE(SUBSTRING(source,posn,1),'0123456789.eE+-')>0 DO SET posn=posn+1; END WHILE;
   SET token=SUBSTRING(source,startn,posn-startn);
   IF NOT (token REGEXP '(?-i)\\A-?(0|[1-9][0-9]*)(\\.[0-9]+)?([eE][+-]?[0-9]+)?\\z') THEN RETURN FALSE; END IF;
   IF LEFT(token,1)='-' THEN SET token=SUBSTRING(token,2); END IF;
   SET en=LOCATE('e',LOWER(token)); SET expo=0;
   IF en>0 THEN
    SET digits=SUBSTRING(token,en+1);
    IF LEFT(digits,1) IN ('-','+') THEN SET digits=SUBSTRING(digits,2); END IF;
    SET digits=TRIM(LEADING '0' FROM digits);
    IF CHAR_LENGTH(digits)>10 THEN RETURN FALSE; END IF;
    IF digits<>'' AND CAST(digits AS UNSIGNED)>1073741823 THEN RETURN FALSE; END IF;
    SET expo=CAST(SUBSTRING(token,en+1) AS SIGNED);
    SET token=LEFT(token,en-1);
   END IF;
   SET dotn=LOCATE('.',token);
   SET scale_n=IF(dotn=0,0,CHAR_LENGTH(token)-dotn)-expo;
   IF scale_n>16383 THEN RETURN FALSE; END IF;
   SET digits=REPLACE(token,'.','');
   SET leading_n=CHAR_LENGTH(digits)-CHAR_LENGTH(TRIM(LEADING '0' FROM digits));
   SET integer_n=IF(dotn=0,CHAR_LENGTH(token),dotn-1)+expo-leading_n;
   IF leading_n<>CHAR_LENGTH(digits) AND integer_n>131072 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,'0');
  ELSE
   SET masked=CONCAT(masked,c); SET posn=posn+1;
  END IF;
 END WHILE;
 IF NOT JSON_VALID(masked) THEN RETURN FALSE; END IF;
 RETURN CAST(JSON_TYPE(masked) AS BINARY)=_binary'ARRAY';
END;
CREATE FUNCTION `ocserv_jsonb_object_valid`(doc LONGBLOB) RETURNS tinyint(1)
    NO SQL
    DETERMINISTIC
BEGIN
 DECLARE source LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE masked LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT '';
 DECLARE token LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE digits LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE c CHAR(1) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE posn BIGINT DEFAULT 1;
 DECLARE startn BIGINT;
 DECLARE n BIGINT;
 DECLARE dotn BIGINT;
 DECLARE en BIGINT;
 DECLARE expo BIGINT;
 DECLARE scale_n BIGINT;
 DECLARE integer_n BIGINT;
 DECLARE leading_n BIGINT;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION RETURN FALSE;
 IF doc IS NULL THEN RETURN FALSE; END IF;
 SET source=CONVERT(doc USING utf8mb4);
 IF CAST(source AS BINARY)<>doc THEN RETURN FALSE; END IF;
 SET n=CHAR_LENGTH(source);
 WHILE posn<=n DO
  SET c=SUBSTRING(source,posn,1);
  IF c='"' THEN
   SET startn=posn;
   SET posn=posn+1;
   WHILE posn<=n AND SUBSTRING(source,posn,1)<>'"' DO
    IF ASCII(SUBSTRING(source,posn,1))=92 THEN SET posn=posn+1; END IF;
    SET posn=posn+1;
   END WHILE;
   IF posn>n THEN RETURN FALSE; END IF;
   SET token=SUBSTRING(source,startn,posn-startn+1);
   IF NOT JSON_VALID(token) OR LOCATE(0x00,CAST(JSON_UNQUOTE(token) AS BINARY))<>0 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,token);
   SET posn=posn+1;
  ELSEIF c='-' OR (ASCII(c)>=48 AND ASCII(c)<=57) THEN
   SET startn=posn;
   WHILE posn<=n AND LOCATE(SUBSTRING(source,posn,1),'0123456789.eE+-')>0 DO SET posn=posn+1; END WHILE;
   SET token=SUBSTRING(source,startn,posn-startn);
   IF NOT (token REGEXP '(?-i)\\A-?(0|[1-9][0-9]*)(\\.[0-9]+)?([eE][+-]?[0-9]+)?\\z') THEN RETURN FALSE; END IF;
   IF LEFT(token,1)='-' THEN SET token=SUBSTRING(token,2); END IF;
   SET en=LOCATE('e',LOWER(token)); SET expo=0;
   IF en>0 THEN
    SET digits=SUBSTRING(token,en+1);
    IF LEFT(digits,1) IN ('-','+') THEN SET digits=SUBSTRING(digits,2); END IF;
    SET digits=TRIM(LEADING '0' FROM digits);
    IF CHAR_LENGTH(digits)>10 THEN RETURN FALSE; END IF;
    IF digits<>'' AND CAST(digits AS UNSIGNED)>1073741823 THEN RETURN FALSE; END IF;
    SET expo=CAST(SUBSTRING(token,en+1) AS SIGNED);
    SET token=LEFT(token,en-1);
   END IF;
   SET dotn=LOCATE('.',token);
   SET scale_n=IF(dotn=0,0,CHAR_LENGTH(token)-dotn)-expo;
   IF scale_n>16383 THEN RETURN FALSE; END IF;
   SET digits=REPLACE(token,'.','');
   SET leading_n=CHAR_LENGTH(digits)-CHAR_LENGTH(TRIM(LEADING '0' FROM digits));
   SET integer_n=IF(dotn=0,CHAR_LENGTH(token),dotn-1)+expo-leading_n;
   IF leading_n<>CHAR_LENGTH(digits) AND integer_n>131072 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,'0');
  ELSE
   SET masked=CONCAT(masked,c); SET posn=posn+1;
  END IF;
 END WHILE;
 IF NOT JSON_VALID(masked) THEN RETURN FALSE; END IF;
 RETURN CAST(JSON_TYPE(masked) AS BINARY)=_binary'OBJECT';
END;
CREATE FUNCTION `ocserv_jsonb_value_valid`(doc LONGBLOB) RETURNS tinyint(1)
    NO SQL
    DETERMINISTIC
BEGIN
 DECLARE source LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE masked LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT '';
 DECLARE token LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE digits LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE c CHAR(1) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE posn BIGINT DEFAULT 1;
 DECLARE startn BIGINT;
 DECLARE n BIGINT;
 DECLARE dotn BIGINT;
 DECLARE en BIGINT;
 DECLARE expo BIGINT;
 DECLARE scale_n BIGINT;
 DECLARE integer_n BIGINT;
 DECLARE leading_n BIGINT;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION RETURN FALSE;
 IF doc IS NULL THEN RETURN FALSE; END IF;
 SET source=CONVERT(doc USING utf8mb4);
 IF CAST(source AS BINARY)<>doc THEN RETURN FALSE; END IF;
 SET n=CHAR_LENGTH(source);
 WHILE posn<=n DO
  SET c=SUBSTRING(source,posn,1);
  IF c='"' THEN
   SET startn=posn;
   SET posn=posn+1;
   WHILE posn<=n AND SUBSTRING(source,posn,1)<>'"' DO
    IF ASCII(SUBSTRING(source,posn,1))=92 THEN SET posn=posn+1; END IF;
    SET posn=posn+1;
   END WHILE;
   IF posn>n THEN RETURN FALSE; END IF;
   SET token=SUBSTRING(source,startn,posn-startn+1);
   IF NOT JSON_VALID(token) OR LOCATE(0x00,CAST(JSON_UNQUOTE(token) AS BINARY))<>0 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,token);
   SET posn=posn+1;
  ELSEIF c='-' OR (ASCII(c)>=48 AND ASCII(c)<=57) THEN
   SET startn=posn;
   WHILE posn<=n AND LOCATE(SUBSTRING(source,posn,1),'0123456789.eE+-')>0 DO SET posn=posn+1; END WHILE;
   SET token=SUBSTRING(source,startn,posn-startn);
   IF NOT (token REGEXP '(?-i)\\A-?(0|[1-9][0-9]*)(\\.[0-9]+)?([eE][+-]?[0-9]+)?\\z') THEN RETURN FALSE; END IF;
   IF LEFT(token,1)='-' THEN SET token=SUBSTRING(token,2); END IF;
   SET en=LOCATE('e',LOWER(token)); SET expo=0;
   IF en>0 THEN
    SET digits=SUBSTRING(token,en+1);
    IF LEFT(digits,1) IN ('-','+') THEN SET digits=SUBSTRING(digits,2); END IF;
    SET digits=TRIM(LEADING '0' FROM digits);
    IF CHAR_LENGTH(digits)>10 THEN RETURN FALSE; END IF;
    IF digits<>'' AND CAST(digits AS UNSIGNED)>1073741823 THEN RETURN FALSE; END IF;
    SET expo=CAST(SUBSTRING(token,en+1) AS SIGNED);
    SET token=LEFT(token,en-1);
   END IF;
   SET dotn=LOCATE('.',token);
   SET scale_n=IF(dotn=0,0,CHAR_LENGTH(token)-dotn)-expo;
   IF scale_n>16383 THEN RETURN FALSE; END IF;
   SET digits=REPLACE(token,'.','');
   SET leading_n=CHAR_LENGTH(digits)-CHAR_LENGTH(TRIM(LEADING '0' FROM digits));
   SET integer_n=IF(dotn=0,CHAR_LENGTH(token),dotn-1)+expo-leading_n;
   IF leading_n<>CHAR_LENGTH(digits) AND integer_n>131072 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,'0');
  ELSE
   SET masked=CONCAT(masked,c); SET posn=posn+1;
  END IF;
 END WHILE;
 IF NOT JSON_VALID(masked) THEN RETURN FALSE; END IF;
 RETURN TRUE;
END;
CREATE FUNCTION `ocserv_rollout_exclusions_valid`(doc LONGBLOB) RETURNS tinyint(1)
    NO SQL
    DETERMINISTIC
BEGIN
 DECLARE source LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE masked LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT '';
 DECLARE token LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE digits LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE c CHAR(1) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE posn BIGINT DEFAULT 1;
 DECLARE startn BIGINT;
 DECLARE n BIGINT;
 DECLARE dotn BIGINT;
 DECLARE en BIGINT;
 DECLARE expo BIGINT;
 DECLARE scale_n BIGINT;
 DECLARE integer_n BIGINT;
 DECLARE leading_n BIGINT;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION RETURN FALSE;
 IF doc IS NULL THEN RETURN FALSE; END IF;
 SET source=CONVERT(doc USING utf8mb4);
 IF CAST(source AS BINARY)<>doc THEN RETURN FALSE; END IF;
 SET n=CHAR_LENGTH(source);
 WHILE posn<=n DO
  SET c=SUBSTRING(source,posn,1);
  IF c='"' THEN
   SET startn=posn;
   SET posn=posn+1;
   WHILE posn<=n AND SUBSTRING(source,posn,1)<>'"' DO
    IF ASCII(SUBSTRING(source,posn,1))=92 THEN SET posn=posn+1; END IF;
    SET posn=posn+1;
   END WHILE;
   IF posn>n THEN RETURN FALSE; END IF;
   SET token=SUBSTRING(source,startn,posn-startn+1);
   IF NOT JSON_VALID(token) OR LOCATE(0x00,CAST(JSON_UNQUOTE(token) AS BINARY))<>0 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,token);
   SET posn=posn+1;
  ELSEIF c='-' OR (ASCII(c)>=48 AND ASCII(c)<=57) THEN
   SET startn=posn;
   WHILE posn<=n AND LOCATE(SUBSTRING(source,posn,1),'0123456789.eE+-')>0 DO SET posn=posn+1; END WHILE;
   SET token=SUBSTRING(source,startn,posn-startn);
   IF NOT (token REGEXP '(?-i)\\A-?(0|[1-9][0-9]*)(\\.[0-9]+)?([eE][+-]?[0-9]+)?\\z') THEN RETURN FALSE; END IF;
   IF LEFT(token,1)='-' THEN SET token=SUBSTRING(token,2); END IF;
   SET en=LOCATE('e',LOWER(token)); SET expo=0;
   IF en>0 THEN
    SET digits=SUBSTRING(token,en+1);
    IF LEFT(digits,1) IN ('-','+') THEN SET digits=SUBSTRING(digits,2); END IF;
    SET digits=TRIM(LEADING '0' FROM digits);
    IF CHAR_LENGTH(digits)>10 THEN RETURN FALSE; END IF;
    IF digits<>'' AND CAST(digits AS UNSIGNED)>1073741823 THEN RETURN FALSE; END IF;
    SET expo=CAST(SUBSTRING(token,en+1) AS SIGNED);
    SET token=LEFT(token,en-1);
   END IF;
   SET dotn=LOCATE('.',token);
   SET scale_n=IF(dotn=0,0,CHAR_LENGTH(token)-dotn)-expo;
   IF scale_n>16383 THEN RETURN FALSE; END IF;
   SET digits=REPLACE(token,'.','');
   SET leading_n=CHAR_LENGTH(digits)-CHAR_LENGTH(TRIM(LEADING '0' FROM digits));
   SET integer_n=IF(dotn=0,CHAR_LENGTH(token),dotn-1)+expo-leading_n;
   IF leading_n<>CHAR_LENGTH(digits) AND integer_n>131072 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,'0');
  ELSE
   SET masked=CONCAT(masked,c); SET posn=posn+1;
  END IF;
 END WHILE;
 IF NOT JSON_VALID(masked) THEN RETURN FALSE; END IF;
 RETURN CAST(JSON_TYPE(masked) AS BINARY)=_binary'ARRAY' AND JSON_LENGTH(masked)<=500;
END;
CREATE FUNCTION `ocserv_text_array_valid`(doc LONGBLOB) RETURNS tinyint(1)
    NO SQL
    DETERMINISTIC
BEGIN
 DECLARE source LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE dims LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE elems LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE item LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE dimension_n BIGINT;
 DECLARE length_n BIGINT;
 DECLARE lower_n BIGINT;
 DECLARE total_n BIGINT DEFAULT 1;
 DECLARE i BIGINT DEFAULT 0;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION RETURN FALSE;
 IF doc IS NULL THEN RETURN FALSE; END IF;
 SET source=CONVERT(doc USING utf8mb4);
 IF CAST(source AS BINARY)<>doc OR NOT JSON_VALID(source) THEN RETURN FALSE; END IF;
 IF CAST(JSON_TYPE(source) AS BINARY)<>_binary'OBJECT' OR JSON_LENGTH(source)<>2 THEN RETURN FALSE; END IF;
 SET dims=JSON_EXTRACT(source,'$.dimensions'); SET elems=JSON_EXTRACT(source,'$.elements');
 IF dims IS NULL OR elems IS NULL OR CAST(JSON_TYPE(dims) AS BINARY)<>_binary'ARRAY' OR CAST(JSON_TYPE(elems) AS BINARY)<>_binary'ARRAY' THEN RETURN FALSE; END IF;
 SET dimension_n=JSON_LENGTH(dims);
 IF dimension_n>6 THEN RETURN FALSE; END IF;
 IF dimension_n=0 THEN SET total_n=0; END IF;
 WHILE i<dimension_n DO
  SET item=JSON_EXTRACT(dims,CONCAT('$[',i,']'));
  IF CAST(JSON_TYPE(item) AS BINARY)<>_binary'OBJECT' OR JSON_LENGTH(item)<>2 OR JSON_EXTRACT(item,'$.length') IS NULL OR JSON_EXTRACT(item,'$.lower_bound') IS NULL THEN RETURN FALSE; END IF;
  IF CAST(JSON_TYPE(JSON_EXTRACT(item,'$.length')) AS BINARY)<>_binary'INTEGER' OR CAST(JSON_TYPE(JSON_EXTRACT(item,'$.lower_bound')) AS BINARY)<>_binary'INTEGER' THEN RETURN FALSE; END IF;
  SET length_n=CAST(JSON_UNQUOTE(JSON_EXTRACT(item,'$.length')) AS SIGNED);
  SET lower_n=CAST(JSON_UNQUOTE(JSON_EXTRACT(item,'$.lower_bound')) AS SIGNED);
  IF length_n<=0 OR length_n>134217727 OR lower_n < -2147483648 OR lower_n>2147483647 OR lower_n+length_n>2147483647 OR total_n>134217727 DIV length_n THEN RETURN FALSE; END IF;
  SET total_n=total_n*length_n; SET i=i+1;
 END WHILE;
 IF JSON_LENGTH(elems)<>total_n THEN RETURN FALSE; END IF;
 SET i=0;
 WHILE i<total_n DO
  SET item=JSON_EXTRACT(elems,CONCAT('$[',i,']'));
  IF CAST(JSON_TYPE(item) AS BINARY) NOT IN (_binary'STRING',_binary'NULL') THEN RETURN FALSE; END IF;
  IF CAST(JSON_TYPE(item) AS BINARY)=_binary'STRING' AND LOCATE(0x00,CAST(JSON_UNQUOTE(item) AS BINARY))<>0 THEN RETURN FALSE; END IF;
  SET i=i+1;
 END WHILE;
 RETURN TRUE;
END;
CREATE PROCEDURE `audit_compact_detail`(IN p_id VARBINARY(16),IN p_hash VARBINARY(32),IN p_key VARBINARY(128),IN p_mac VARBINARY(32),IN p_at BIGINT)
BEGIN UPDATE audit_events SET reason=NULL,before_summary=NULL,after_summary=NULL,details_compacted_at=p_at,compaction_key_id=p_key,compaction_mac=p_mac WHERE id=p_id AND event_hash=p_hash AND details_compacted_at IS NULL AND auth_version=1 AND BINARY action<>BINARY 'audit.auth.transition' AND occurred_at<=p_at-7776000000000 AND p_at<=CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED); SELECT ROW_COUNT()=1; END;
CREATE PROCEDURE `security_compact_details`(IN p_cutoff BIGINT)
BEGIN UPDATE telemetry_security_events SET detail_sha256=UNHEX(SHA2(detail,256)),detail='{}',details_compacted_at=CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED) WHERE details_compacted_at IS NULL AND observed_at<LEAST(p_cutoff,CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED)-7776000000000) ORDER BY observed_at,event_id LIMIT 32; END;
CREATE PROCEDURE `telemetry_prune_rollups`(IN maintenance_time BIGINT)
BEGIN
 DECLARE clock_at BIGINT;
 DECLARE cut_hour BIGINT;
 SET clock_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6));
 IF maintenance_time IS NULL OR maintenance_time<clock_at-300000000 OR maintenance_time>clock_at+300000000 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry maintenance clock outside permitted window';
 END IF;
 SET maintenance_time=LEAST(maintenance_time,clock_at);
 SET cut_hour=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',DATE_SUB(TIMESTAMPADD(MICROSECOND,maintenance_time,'2000-01-01'),INTERVAL 13 MONTH));
 DELETE FROM telemetry_rollups_5m WHERE bucket_at<maintenance_time-7776000000000 ORDER BY bucket_at,exact_row_id LIMIT 1000;
 DELETE FROM telemetry_rollups_1h WHERE bucket_at<cut_hour ORDER BY bucket_at,exact_row_id LIMIT 1000;
END;
CREATE PROCEDURE `telemetry_retire_shards`(IN requested_cutoff BIGINT)
main: BEGIN
 DECLARE clock_at BIGINT;
 DECLARE phase_no INT;
 DECLARE cut_at BIGINT;
 DECLARE since_at BIGINT;
 DECLARE upper_at BIGINT;
 DECLARE stop_at BIGINT;
 DECLARE cursor_at BIGINT;
 DECLARE cursor_node VARBINARY(16);
 DECLARE cursor_metric LONGBLOB;
 DECLARE candidate VARBINARY(64);
 DECLARE guard_key VARBINARY(128);
 DECLARE migration_state VARBINARY(16);
 DECLARE sources LONGTEXT;
 DECLARE aggregate_sql LONGTEXT;
 DECLARE target_table VARCHAR(64);
 DECLARE page_count INT;
 DECLARE statement_open BOOLEAN DEFAULT FALSE;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION
 BEGIN
  IF statement_open THEN DEALLOCATE PREPARE telemetry_batch; END IF;
  SET @telemetry_batch_sql=NULL, @telemetry_batch_matches=NULL;
  RESIGNAL;
 END;
 SET clock_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6));
 IF requested_cutoff IS NULL OR requested_cutoff>clock_at-1209600000000+300000000 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry retention cutoff outside permitted window';
 END IF;
 SAVEPOINT ocservia_telemetry_batch_tx;
 RELEASE SAVEPOINT ocservia_telemetry_batch_tx;
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 SELECT state INTO migration_state FROM telemetry_legacy_migration WHERE singleton=1 LOCK IN SHARE MODE;
 IF guard_key IS NULL OR migration_state IS NULL OR migration_state<>'complete' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry history migration is incomplete';
 END IF;
 SELECT phase,p.cutoff,p.candidate,p.cursor_at,p.cursor_node,p.cursor_metric
 INTO phase_no,cut_at,candidate,cursor_at,cursor_node,cursor_metric
 FROM telemetry_maintenance_progress p WHERE singleton=1 FOR UPDATE;
 IF phase_no=0 THEN
  SET cut_at=LEAST(requested_cutoff,clock_at-1209600000000);
  SET candidate=NULL;
  SELECT table_name INTO candidate FROM telemetry_sample_shards FORCE INDEX (telemetry_retirement_lookup)
  WHERE state='active' AND end_at<=cut_at ORDER BY start_at LIMIT 1;
  SET phase_no=1, cursor_at=NULL, cursor_node=NULL, cursor_metric=NULL;
  UPDATE telemetry_maintenance_progress p SET phase=1,cutoff=cut_at,p.candidate=candidate,
   cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
 END IF;
 phases: WHILE phase_no<=4 DO
  IF phase_no>=3 AND candidate IS NULL THEN LEAVE phases; END IF;
  SET target_table=IF(MOD(phase_no,2)=1,'telemetry_rollups_5m','telemetry_rollups_1h');
  IF phase_no<=2 THEN
   SET since_at=FLOOR(GREATEST(cut_at,clock_at-1209600000000)/IF(phase_no=1,300000000,3600000000))*IF(phase_no=1,300000000,3600000000);
  ELSEIF phase_no=3 THEN
   SET since_at=FLOOR((clock_at-7776000000000)/300000000)*300000000;
  ELSE
   SET since_at=FLOOR(TIMESTAMPDIFF(MICROSECOND,'2000-01-01',DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 13 MONTH))/3600000000)*3600000000;
  END IF;
  IF cursor_at IS NOT NULL THEN SET since_at=GREATEST(since_at,cursor_at); END IF;
  SET stop_at=GREATEST(cut_at+1209600000000,clock_at)+300000000;
  IF phase_no>=3 THEN SELECT GREATEST(since_at,start_at),end_at INTO since_at,stop_at FROM telemetry_sample_shards WHERE table_name=candidate; END IF;
  SET upper_at=9223372036854775807;
  IF since_at<stop_at-86400000000 THEN SET upper_at=since_at+86400000000; END IF;
  SET sources='';
  IF phase_no<=2 THEN
   SET sources=CONCAT('SELECT node_id,metric,sampled_at,value FROM telemetry_samples WHERE sampled_at>=',since_at,' AND sampled_at<=',upper_at,IF(upper_at=9223372036854775807,'','-1'));
   BEGIN
    DECLARE finished BOOLEAN DEFAULT FALSE;
    DECLARE shard VARBINARY(64);
    DECLARE shards CURSOR FOR SELECT table_name FROM telemetry_sample_shards WHERE state='active' AND end_at>since_at ORDER BY start_at;
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET finished=TRUE;
    OPEN shards;
    shard_loop: LOOP
     FETCH shards INTO shard;
     IF finished THEN LEAVE shard_loop; END IF;
     IF CONVERT(shard USING ascii) NOT REGEXP '^telemetry_samples_(m_[0-9]{6}|x_[pn][0-9]{7})$' THEN
      SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='invalid telemetry shard name';
     END IF;
     SET sources=CONCAT(sources,' UNION ALL SELECT node_id,metric,sampled_at,value FROM ',shard,' WHERE sampled_at>=',since_at,' AND sampled_at<=',upper_at,IF(upper_at=9223372036854775807,'','-1'));
    END LOOP;
    CLOSE shards;
   END;
  ELSE
   IF CONVERT(candidate USING ascii) NOT REGEXP '^telemetry_samples_(m_[0-9]{6}|x_[pn][0-9]{7})$' THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='invalid telemetry shard name';
   END IF;
   SET sources=CONCAT('SELECT node_id,metric,sampled_at,value FROM ',candidate,' WHERE sampled_at>=',since_at,' AND sampled_at<=',upper_at,IF(upper_at=9223372036854775807,'','-1'));
  END IF;
  SET aggregate_sql=IF(MOD(phase_no,2)=1,CONCAT('SELECT node_id,metric,CASE WHEN sampled_at IN (-9223372036854775808,9223372036854775807) THEN sampled_at ELSE FLOOR(CAST(sampled_at AS DECIMAL(20,0))/300000000)*300000000 END AS bucket_at,COUNT(*) AS sample_count,MIN(value) AS min_value,MAX(value) AS max_value,AVG(value) AS avg_value FROM (',sources,') AS samples GROUP BY node_id,metric,bucket_at'),CONCAT('SELECT node_id,metric,CASE WHEN sampled_at IN (-9223372036854775808,9223372036854775807) THEN sampled_at ELSE FLOOR(CAST(sampled_at AS DECIMAL(20,0))/3600000000)*3600000000 END AS bucket_at,COUNT(*) AS sample_count,MIN(value) AS min_value,MAX(value) AS max_value,AVG(value) AS avg_value FROM (',sources,') AS samples GROUP BY node_id,metric,bucket_at'));
  DELETE FROM telemetry_maintenance_page;
  SET @telemetry_batch_sql=CONCAT('INSERT INTO telemetry_maintenance_page SELECT ROW_NUMBER() OVER(ORDER BY bucket_at,node_id,BINARY metric),page.* FROM (SELECT a.* FROM (',aggregate_sql,') a LEFT JOIN ',target_table,
   ' r ON r.node_id=a.node_id AND BINARY r.metric=BINARY a.metric AND r.bucket_at=a.bucket_at WHERE (r.exact_row_id IS NULL OR NOT(r.sample_count <=> a.sample_count AND r.min_value <=> a.min_value AND r.max_value <=> a.max_value AND r.avg_value <=> a.avg_value))');
  IF cursor_node IS NOT NULL THEN
   SET @telemetry_batch_sql=CONCAT(@telemetry_batch_sql,' AND (a.bucket_at>',cursor_at,
    ' OR (a.bucket_at=',cursor_at,' AND (a.node_id>UNHEX(''',HEX(cursor_node),''')',
    ' OR (a.node_id=UNHEX(''',HEX(cursor_node),''') AND BINARY a.metric>UNHEX(''',HEX(cursor_metric),''')))))');
  END IF;
  SET @telemetry_batch_sql=CONCAT(@telemetry_batch_sql,' ORDER BY a.bucket_at,a.node_id,BINARY a.metric LIMIT 80) page');
  PREPARE telemetry_batch FROM @telemetry_batch_sql;
  SET statement_open=TRUE;
  EXECUTE telemetry_batch;
  DEALLOCATE PREPARE telemetry_batch;
  SET statement_open=FALSE;
  SELECT COUNT(*) INTO page_count FROM telemetry_maintenance_page;
  IF page_count>0 THEN
   
   SET guard_key=NULL;
   SELECT key_name INTO guard_key FROM exact_key_guards WHERE key_name=target_table FOR UPDATE;
   IF guard_key IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF;
   IF NOT EXISTS(SELECT 1 FROM telemetry_maintenance_page WHERE OCTET_LENGTH(metric)>255) THEN
SET @telemetry_batch_sql=CONCAT('INSERT INTO ',target_table,'(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value FROM telemetry_maintenance_page WHERE TRUE ON DUPLICATE KEY UPDATE sample_count=VALUES(sample_count),min_value=VALUES(min_value),max_value=VALUES(max_value),avg_value=VALUES(avg_value)');
PREPARE telemetry_batch FROM @telemetry_batch_sql;
SET statement_open=TRUE;
EXECUTE telemetry_batch;
DEALLOCATE PREPARE telemetry_batch;
SET statement_open=FALSE;
ELSE
SET @telemetry_batch_sql=CONCAT('UPDATE ',target_table,' r JOIN (SELECT node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value FROM telemetry_maintenance_page) a ON r.node_id=a.node_id AND BINARY r.metric=BINARY a.metric AND r.bucket_at=a.bucket_at SET r.sample_count=a.sample_count,r.min_value=a.min_value,r.max_value=a.max_value,r.avg_value=a.avg_value WHERE NOT (r.sample_count <=> a.sample_count AND r.min_value <=> a.min_value AND r.max_value <=> a.max_value AND r.avg_value <=> a.avg_value)');
PREPARE telemetry_batch FROM @telemetry_batch_sql;
SET statement_open=TRUE;
EXECUTE telemetry_batch;
DEALLOCATE PREPARE telemetry_batch;
SET statement_open=FALSE;
SET @telemetry_batch_sql=CONCAT('INSERT INTO ',target_table,'(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT a.node_id,a.metric,a.bucket_at,a.sample_count,a.min_value,a.max_value,a.avg_value FROM (SELECT node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value FROM telemetry_maintenance_page) a LEFT JOIN ',target_table,' r ON r.node_id=a.node_id AND BINARY r.metric=BINARY a.metric AND r.bucket_at=a.bucket_at WHERE r.exact_row_id IS NULL');
PREPARE telemetry_batch FROM @telemetry_batch_sql;
SET statement_open=TRUE;
EXECUTE telemetry_batch;
DEALLOCATE PREPARE telemetry_batch;
SET statement_open=FALSE;
END IF;

   SELECT p.bucket_at,p.node_id,p.metric INTO cursor_at,cursor_node,cursor_metric FROM telemetry_maintenance_page p ORDER BY slot DESC LIMIT 1;
   UPDATE telemetry_maintenance_progress p SET p.cursor_at=cursor_at,p.cursor_node=cursor_node,p.cursor_metric=cursor_metric WHERE singleton=1;
   SET @telemetry_batch_sql=NULL;
   SELECT FALSE AS done;
   LEAVE main;
  END IF;
  IF upper_at<stop_at THEN
   SET cursor_at=upper_at,cursor_node=NULL,cursor_metric=NULL;
   UPDATE telemetry_maintenance_progress p SET p.cursor_at=cursor_at,p.cursor_node=NULL,p.cursor_metric=NULL WHERE singleton=1;
   ITERATE phases;
  END IF;
  SET phase_no=phase_no+1,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL;
  UPDATE telemetry_maintenance_progress SET phase=phase_no,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
 END WHILE;
 IF candidate IS NOT NULL THEN
  SELECT table_name INTO guard_key FROM telemetry_sample_shards WHERE table_name=candidate AND state='active' FOR UPDATE;
  SET since_at=FLOOR((clock_at-7776000000000)/300000000)*300000000;
SET @telemetry_batch_sql=CONCAT('SELECT NOT EXISTS(SELECT 1 FROM (SELECT node_id,metric,CASE WHEN sampled_at IN (-9223372036854775808,9223372036854775807) THEN sampled_at ELSE FLOOR(CAST(sampled_at AS DECIMAL(20,0))/300000000)*300000000 END AS bucket_at,COUNT(*) AS sample_count,MIN(value) AS min_value,MAX(value) AS max_value,AVG(value) AS avg_value FROM (SELECT node_id,metric,sampled_at,value FROM `',candidate,'` WHERE sampled_at>=',since_at,') AS samples GROUP BY node_id,metric,bucket_at) a LEFT JOIN telemetry_rollups_5m r ON r.node_id=a.node_id AND BINARY r.metric=BINARY a.metric AND r.bucket_at=a.bucket_at WHERE r.exact_row_id IS NULL OR NOT(r.sample_count <=> a.sample_count AND r.min_value <=> a.min_value AND r.max_value <=> a.max_value AND r.avg_value <=> a.avg_value)) INTO @telemetry_batch_matches');
PREPARE telemetry_batch FROM @telemetry_batch_sql;
SET statement_open=TRUE;
EXECUTE telemetry_batch;
DEALLOCATE PREPARE telemetry_batch;
SET statement_open=FALSE;

  IF NOT @telemetry_batch_matches THEN
   UPDATE telemetry_maintenance_progress SET phase=3,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
   SET @telemetry_batch_sql=NULL,@telemetry_batch_matches=NULL;
   SELECT FALSE AS done;
   LEAVE main;
  END IF;
SET since_at=FLOOR(TIMESTAMPDIFF(MICROSECOND,'2000-01-01',DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 13 MONTH))/3600000000)*3600000000;
SET @telemetry_batch_sql=CONCAT('SELECT NOT EXISTS(SELECT 1 FROM (SELECT node_id,metric,CASE WHEN sampled_at IN (-9223372036854775808,9223372036854775807) THEN sampled_at ELSE FLOOR(CAST(sampled_at AS DECIMAL(20,0))/3600000000)*3600000000 END AS bucket_at,COUNT(*) AS sample_count,MIN(value) AS min_value,MAX(value) AS max_value,AVG(value) AS avg_value FROM (SELECT node_id,metric,sampled_at,value FROM `',candidate,'` WHERE sampled_at>=',since_at,') AS samples GROUP BY node_id,metric,bucket_at) a LEFT JOIN telemetry_rollups_1h r ON r.node_id=a.node_id AND BINARY r.metric=BINARY a.metric AND r.bucket_at=a.bucket_at WHERE r.exact_row_id IS NULL OR NOT(r.sample_count <=> a.sample_count AND r.min_value <=> a.min_value AND r.max_value <=> a.max_value AND r.avg_value <=> a.avg_value)) INTO @telemetry_batch_matches');
PREPARE telemetry_batch FROM @telemetry_batch_sql;
SET statement_open=TRUE;
EXECUTE telemetry_batch;
DEALLOCATE PREPARE telemetry_batch;
SET statement_open=FALSE;

  IF NOT @telemetry_batch_matches THEN
   UPDATE telemetry_maintenance_progress SET phase=3,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
   SET @telemetry_batch_sql=NULL,@telemetry_batch_matches=NULL;
   SELECT FALSE AS done;
   LEAVE main;
  END IF;

  UPDATE telemetry_sample_shards SET state='retired' WHERE table_name=candidate AND state='active';
 END IF;
 UPDATE telemetry_maintenance_progress SET phase=0,candidate=NULL,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
 DELETE FROM telemetry_maintenance_page;
 SET @telemetry_batch_sql=NULL,@telemetry_batch_matches=NULL;
 SELECT TRUE AS done;
END;
CREATE TRIGGER `approval_summary_insert` BEFORE INSERT ON `approval_requests` FOR EACH ROW BEGIN IF NEW.request_summary IS NOT NULL AND (NOT ocserv_jsonb_value_valid(NEW.request_summary) OR LEFT(TRIM(REPLACE(REPLACE(REPLACE(CONVERT(NEW.request_summary USING utf8mb4),CHAR(9),' '),CHAR(10),' '),CHAR(13),' ')),1) NOT IN ('[','{')) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid approval JSON'; END IF; END;
CREATE TRIGGER `approval_summary_update` BEFORE UPDATE ON `approval_requests` FOR EACH ROW BEGIN IF NEW.request_summary IS NOT NULL AND (NOT ocserv_jsonb_value_valid(NEW.request_summary) OR LEFT(TRIM(REPLACE(REPLACE(REPLACE(CONVERT(NEW.request_summary USING utf8mb4),CHAR(9),' '),CHAR(10),' '),CHAR(13),' ')),1) NOT IN ('[','{')) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid approval JSON'; END IF; END;
CREATE TRIGGER `audit_checkpoints_reject_delete` BEFORE DELETE ON `audit_checkpoints` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='append-only audit record';
CREATE TRIGGER `audit_checkpoints_reject_update` BEFORE UPDATE ON `audit_checkpoints` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='append-only audit record';
CREATE TRIGGER `audit_events_reject_delete` BEFORE DELETE ON `audit_events` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='append-only audit record';
CREATE TRIGGER `audit_events_reject_update` BEFORE UPDATE ON `audit_events` FOR EACH ROW BEGIN IF NOT (
 OLD.details_compacted_at IS NULL AND OLD.auth_version=1 AND BINARY OLD.action<>BINARY 'audit.auth.transition'
 AND NEW.details_compacted_at IS NOT NULL AND OLD.occurred_at<=NEW.details_compacted_at-7776000000000
 AND NEW.details_compacted_at<=CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED)
 AND NEW.reason IS NULL AND NEW.before_summary IS NULL AND NEW.after_summary IS NULL
 AND (CAST(NEW.`id` AS BINARY)<=>CAST(OLD.`id` AS BINARY)) AND (CAST(NEW.`workspace_id` AS BINARY)<=>CAST(OLD.`workspace_id` AS BINARY)) AND (CAST(NEW.`occurred_at` AS BINARY)<=>CAST(OLD.`occurred_at` AS BINARY)) AND (CAST(NEW.`actor_type` AS BINARY)<=>CAST(OLD.`actor_type` AS BINARY)) AND (CAST(NEW.`actor_id` AS BINARY)<=>CAST(OLD.`actor_id` AS BINARY)) AND (CAST(NEW.`source_session_id` AS BINARY)<=>CAST(OLD.`source_session_id` AS BINARY)) AND (CAST(NEW.`action` AS BINARY)<=>CAST(OLD.`action` AS BINARY)) AND (CAST(NEW.`resource_type` AS BINARY)<=>CAST(OLD.`resource_type` AS BINARY)) AND (CAST(NEW.`resource_id` AS BINARY)<=>CAST(OLD.`resource_id` AS BINARY)) AND (CAST(NEW.`node_id` AS BINARY)<=>CAST(OLD.`node_id` AS BINARY)) AND (CAST(NEW.`request_id` AS BINARY)<=>CAST(OLD.`request_id` AS BINARY)) AND (CAST(NEW.`trace_id` AS BINARY)<=>CAST(OLD.`trace_id` AS BINARY)) AND (CAST(NEW.`command_id` AS BINARY)<=>CAST(OLD.`command_id` AS BINARY)) AND (CAST(NEW.`approval_id` AS BINARY)<=>CAST(OLD.`approval_id` AS BINARY)) AND (CAST(NEW.`result` AS BINARY)<=>CAST(OLD.`result` AS BINARY)) AND (CAST(NEW.`error_type` AS BINARY)<=>CAST(OLD.`error_type` AS BINARY)) AND (CAST(NEW.`previous_event_hash` AS BINARY)<=>CAST(OLD.`previous_event_hash` AS BINARY)) AND (CAST(NEW.`event_hash` AS BINARY)<=>CAST(OLD.`event_hash` AS BINARY)) AND (CAST(NEW.`auth_version` AS BINARY)<=>CAST(OLD.`auth_version` AS BINARY)) AND (CAST(NEW.`event_key_id` AS BINARY)<=>CAST(OLD.`event_key_id` AS BINARY)) AND (CAST(NEW.`event_mac` AS BINARY)<=>CAST(OLD.`event_mac` AS BINARY))) THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='audit permits only authenticated aged detail compaction'; END IF; END;
CREATE TRIGGER `audit_jsonb_insert` BEFORE INSERT ON `audit_events` FOR EACH ROW BEGIN IF (NEW.before_summary IS NOT NULL AND NOT ocserv_jsonb_value_valid(NEW.before_summary)) OR (NEW.after_summary IS NOT NULL AND NOT ocserv_jsonb_value_valid(NEW.after_summary)) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid audit JSON'; END IF; END;
CREATE TRIGGER `certificate_jsonb_insert` BEFORE INSERT ON `certificates` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.dns_names) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid certificate JSON array'; END IF; END;
CREATE TRIGGER `certificate_jsonb_update` BEFORE UPDATE ON `certificates` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.dns_names) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid certificate JSON array'; END IF; END;
CREATE TRIGGER `config_plan_jsonb_insert` BEFORE INSERT ON `config_plans` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.warnings) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid configuration warnings JSON array'; END IF; END;
CREATE TRIGGER `config_plan_jsonb_update` BEFORE UPDATE ON `config_plans` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.warnings) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid configuration warnings JSON array'; END IF; END;
CREATE TRIGGER `desired_groups_logical_insert` BEFORE INSERT ON `desired_groups` FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.members) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
CREATE TRIGGER `desired_groups_logical_update` BEFORE UPDATE ON `desired_groups` FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.members) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
CREATE TRIGGER `exact_agent_command_results_insert` AFTER INSERT ON `agent_command_results` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='agent_command_results' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF NEW.`receipt_verification_status` = 'verified' AND NEW.`privd_attestation_key_id` IS NOT NULL AND NEW.`effect_record_id` IS NOT NULL AND NEW.`effect_sequence` IS NOT NULL THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`privd_attestation_key_id` AS BINARY))),16,'0')),CAST(NEW.`privd_attestation_key_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`effect_record_id` AS BINARY))),16,'0')),CAST(NEW.`effect_record_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`effect_sequence` AS BINARY))),16,'0')),CAST(NEW.`effect_sequence` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_agent_command_results` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_agent_command_results` (owner_id,key_value) VALUES(NEW.`event_id`,encoded); END IF; END;
CREATE TRIGGER `exact_agent_command_results_update` AFTER UPDATE ON `agent_command_results` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='agent_command_results' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_agent_command_results` WHERE owner_id=NEW.`event_id`; IF NEW.`receipt_verification_status` = 'verified' AND NEW.`privd_attestation_key_id` IS NOT NULL AND NEW.`effect_record_id` IS NOT NULL AND NEW.`effect_sequence` IS NOT NULL THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`privd_attestation_key_id` AS BINARY))),16,'0')),CAST(NEW.`privd_attestation_key_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`effect_record_id` AS BINARY))),16,'0')),CAST(NEW.`effect_record_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`effect_sequence` AS BINARY))),16,'0')),CAST(NEW.`effect_sequence` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_agent_command_results` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_agent_command_results` (owner_id,key_value) VALUES(NEW.`event_id`,encoded); END IF; END;
CREATE TRIGGER `exact_identities_insert` AFTER INSERT ON `identities` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='identities' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`issuer` AS BINARY))),16,'0')),CAST(NEW.`issuer` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`subject` AS BINARY))),16,'0')),CAST(NEW.`subject` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_identities` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_identities` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_identities_update` AFTER UPDATE ON `identities` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='identities' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_identities` WHERE owner_id=NEW.`id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`issuer` AS BINARY))),16,'0')),CAST(NEW.`issuer` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`subject` AS BINARY))),16,'0')),CAST(NEW.`subject` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_identities` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_identities` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_key_guards_immutable` BEFORE UPDATE ON `exact_key_guards` FOR EACH ROW SIGNAL SQLSTATE '42000' SET MYSQL_ERRNO=1142,MESSAGE_TEXT='exact-key guards are immutable';
CREATE TRIGGER `exact_nodes_insert` AFTER INSERT ON `nodes` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='nodes' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`workspace_id` AS BINARY))),16,'0')),CAST(NEW.`workspace_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`name` AS BINARY))),16,'0')),CAST(NEW.`name` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_nodes` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_nodes` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_nodes_update` AFTER UPDATE ON `nodes` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='nodes' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_nodes` WHERE owner_id=NEW.`id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`workspace_id` AS BINARY))),16,'0')),CAST(NEW.`workspace_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`name` AS BINARY))),16,'0')),CAST(NEW.`name` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_nodes` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_nodes` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_operations_insert` AFTER INSERT ON `operations` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='operations' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF NEW.`idempotency_key` IS NOT NULL THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`workspace_id` AS BINARY))),16,'0')),CAST(NEW.`workspace_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`idempotency_key` AS BINARY))),16,'0')),CAST(NEW.`idempotency_key` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_operations` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_operations` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_operations_update` AFTER UPDATE ON `operations` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='operations' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_operations` WHERE owner_id=NEW.`id`; IF NEW.`idempotency_key` IS NOT NULL THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`workspace_id` AS BINARY))),16,'0')),CAST(NEW.`workspace_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`idempotency_key` AS BINARY))),16,'0')),CAST(NEW.`idempotency_key` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_operations` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_operations` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_telemetry_rollups_1h_insert` AFTER INSERT ON `telemetry_rollups_1h` FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='telemetry_rollups_1h' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`metric` AS BINARY))),16,'0')),CAST(NEW.`metric` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`bucket_at` AS BINARY))),16,'0')),CAST(NEW.`bucket_at` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM exact_telemetry_rollups_1h WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO exact_telemetry_rollups_1h(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END;
CREATE TRIGGER `exact_telemetry_rollups_1h_update` AFTER UPDATE ON `telemetry_rollups_1h` FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; IF NEW.node_id=OLD.node_id AND BINARY NEW.metric=BINARY OLD.metric AND NEW.bucket_at=OLD.bucket_at THEN LEAVE main; END IF; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='telemetry_rollups_1h' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM exact_telemetry_rollups_1h WHERE owner_id=NEW.exact_row_id; IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`metric` AS BINARY))),16,'0')),CAST(NEW.`metric` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`bucket_at` AS BINARY))),16,'0')),CAST(NEW.`bucket_at` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM exact_telemetry_rollups_1h WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO exact_telemetry_rollups_1h(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END;
CREATE TRIGGER `exact_telemetry_rollups_5m_insert` AFTER INSERT ON `telemetry_rollups_5m` FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='telemetry_rollups_5m' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`metric` AS BINARY))),16,'0')),CAST(NEW.`metric` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`bucket_at` AS BINARY))),16,'0')),CAST(NEW.`bucket_at` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM exact_telemetry_rollups_5m WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO exact_telemetry_rollups_5m(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END;
CREATE TRIGGER `exact_telemetry_rollups_5m_update` AFTER UPDATE ON `telemetry_rollups_5m` FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; IF NEW.node_id=OLD.node_id AND BINARY NEW.metric=BINARY OLD.metric AND NEW.bucket_at=OLD.bucket_at THEN LEAVE main; END IF; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='telemetry_rollups_5m' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM exact_telemetry_rollups_5m WHERE owner_id=NEW.exact_row_id; IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`metric` AS BINARY))),16,'0')),CAST(NEW.`metric` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`bucket_at` AS BINARY))),16,'0')),CAST(NEW.`bucket_at` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM exact_telemetry_rollups_5m WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO exact_telemetry_rollups_5m(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END;
CREATE TRIGGER `exact_upstream_sync_records_insert` AFTER INSERT ON `upstream_sync_records` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='upstream_sync_records' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`repository` AS BINARY))),16,'0')),CAST(NEW.`repository` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`old_commit` AS BINARY))),16,'0')),CAST(NEW.`old_commit` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`new_commit` AS BINARY))),16,'0')),CAST(NEW.`new_commit` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_upstream_sync_records` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_upstream_sync_records` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_upstream_sync_records_update` AFTER UPDATE ON `upstream_sync_records` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='upstream_sync_records' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_upstream_sync_records` WHERE owner_id=NEW.`id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`repository` AS BINARY))),16,'0')),CAST(NEW.`repository` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`old_commit` AS BINARY))),16,'0')),CAST(NEW.`old_commit` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`new_commit` AS BINARY))),16,'0')),CAST(NEW.`new_commit` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_upstream_sync_records` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_upstream_sync_records` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_user_policy_enforcements_insert` AFTER INSERT ON `user_policy_enforcements` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='user_policy_enforcements' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`username` AS BINARY))),16,'0')),CAST(NEW.`username` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`policy_version` AS BINARY))),16,'0')),CAST(NEW.`policy_version` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`cause` AS BINARY))),16,'0')),CAST(NEW.`cause` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`period_start` AS BINARY))),16,'0')),CAST(NEW.`period_start` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_user_policy_enforcements` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_user_policy_enforcements` (owner_id,key_value) VALUES(NEW.`exact_row_id`,encoded); END IF; END;
CREATE TRIGGER `exact_user_policy_enforcements_update` AFTER UPDATE ON `user_policy_enforcements` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='user_policy_enforcements' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_user_policy_enforcements` WHERE owner_id=NEW.`exact_row_id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`username` AS BINARY))),16,'0')),CAST(NEW.`username` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`policy_version` AS BINARY))),16,'0')),CAST(NEW.`policy_version` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`cause` AS BINARY))),16,'0')),CAST(NEW.`cause` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`period_start` AS BINARY))),16,'0')),CAST(NEW.`period_start` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_user_policy_enforcements` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_user_policy_enforcements` (owner_id,key_value) VALUES(NEW.`exact_row_id`,encoded); END IF; END;
CREATE TRIGGER `exact_workspaces_insert` AFTER INSERT ON `workspaces` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='workspaces' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`slug` AS BINARY))),16,'0')),CAST(NEW.`slug` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_workspaces` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_workspaces` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `exact_workspaces_update` AFTER UPDATE ON `workspaces` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='workspaces' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_workspaces` WHERE owner_id=NEW.`id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`slug` AS BINARY))),16,'0')),CAST(NEW.`slug` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_workspaces` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_workspaces` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
CREATE TRIGGER `nodes_jsonb_insert` BEFORE INSERT ON `nodes` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_value_valid(NEW.`labels`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END;
CREATE TRIGGER `nodes_jsonb_update` BEFORE UPDATE ON `nodes` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_value_valid(NEW.`labels`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END;
CREATE TRIGGER `node_snapshot_jsonb_insert` BEFORE INSERT ON `node_observed_snapshots` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.ocserv) OR NOT ocserv_jsonb_object_valid(NEW.system) OR NOT ocserv_jsonb_object_valid(NEW.path) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid snapshot JSON object'; END IF; END;
CREATE TRIGGER `node_snapshot_jsonb_update` BEFORE UPDATE ON `node_observed_snapshots` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.ocserv) OR NOT ocserv_jsonb_object_valid(NEW.system) OR NOT ocserv_jsonb_object_valid(NEW.path) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid snapshot JSON object'; END IF; END;
CREATE TRIGGER `observed_groups_logical_insert` BEFORE INSERT ON `observed_groups` FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.`members`) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
CREATE TRIGGER `observed_groups_logical_update` BEFORE UPDATE ON `observed_groups` FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.`members`) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
CREATE TRIGGER `rollout_jsonb_insert` BEFORE INSERT ON `agent_rollouts` FOR EACH ROW BEGIN IF NOT ocserv_rollout_exclusions_valid(NEW.exclusions) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid rollout exclusions JSON array'; END IF; END;
CREATE TRIGGER `rollout_jsonb_update` BEFORE UPDATE ON `agent_rollouts` FOR EACH ROW BEGIN IF NOT ocserv_rollout_exclusions_valid(NEW.exclusions) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid rollout exclusions JSON array'; END IF; END;
CREATE TRIGGER `telemetry_legacy_insert_guard` BEFORE INSERT ON `telemetry_samples` FOR EACH ROW BEGIN
 DECLARE guard_key VARBINARY(128);
 DECLARE migration_state VARBINARY(16);
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 SELECT state INTO migration_state FROM telemetry_legacy_migration WHERE singleton=1 LOCK IN SHARE MODE;
 IF guard_key IS NULL OR migration_state IS NULL THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry legacy migration metadata is unavailable';
 END IF;
 IF migration_state='complete' AND NEW.sampled_at NOT IN (-9223372036854775808,9223372036854775807) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='finite telemetry must use its monthly shard';
 END IF;
END;
CREATE TRIGGER `telemetry_legacy_update_guard` BEFORE UPDATE ON `telemetry_samples` FOR EACH ROW BEGIN
 DECLARE guard_key VARBINARY(128);
 DECLARE migration_state VARBINARY(16);
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 SELECT state INTO migration_state FROM telemetry_legacy_migration WHERE singleton=1 LOCK IN SHARE MODE;
 IF guard_key IS NULL OR migration_state IS NULL THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry legacy migration metadata is unavailable';
 END IF;
 IF migration_state='complete' AND NEW.sampled_at NOT IN (-9223372036854775808,9223372036854775807) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='finite telemetry must use its monthly shard';
 END IF;
END;
CREATE TRIGGER `telemetry_security_events_logical_insert` BEFORE INSERT ON `telemetry_security_events` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.`detail`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
CREATE TRIGGER `telemetry_security_events_logical_update` BEFORE UPDATE ON `telemetry_security_events` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.`detail`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
CREATE TRIGGER `upstream_sync_records_jsonb_insert` BEFORE INSERT ON `upstream_sync_records` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.`classification`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END;
CREATE TRIGGER `upstream_sync_records_jsonb_update` BEFORE UPDATE ON `upstream_sync_records` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.`classification`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END;
