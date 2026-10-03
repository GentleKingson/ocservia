-- ocservia:artifact=schema
-- ocservia:format=1
-- ocservia:engine=mysql
-- ocservia:epoch=1
-- ocservia:revision=0

-- ocservia:step=001:table_backend_schema_snapshot
-- ocservia:metadata={"kind":"table","object":"backend_schema_snapshot","before":"","after":"1393fe2017259a7a2092eaa7fb8a034129c370e56bd2855329d1e83a2c0e9dca"}
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
-- ocservia:end-step
-- ocservia:step=002:table_backend_schema_snapshot_steps
-- ocservia:metadata={"kind":"table","object":"backend_schema_snapshot_steps","before":"","after":"75c83bc18c530f480467dc587620788058e17058eae01112247234726aa2d9d2"}
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
-- ocservia:end-step
-- ocservia:step=003:table_backend_migration_steps
-- ocservia:metadata={"kind":"table","object":"backend_migration_steps","before":"","after":"fe839e0287e815c1ee67838423b8442a92949f19d0c2b87460582c76a27aef3a"}
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
-- ocservia:end-step
-- ocservia:step=004:table_backend_migrations
-- ocservia:metadata={"kind":"table","object":"backend_migrations","before":"","after":"50c201717d95567e886862166579551ad52c77bf452bd5714efb91942a49079b"}
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
-- ocservia:end-step
-- ocservia:step=005:table_backend_schema_revisions
-- ocservia:metadata={"kind":"table","object":"backend_schema_revisions","before":"","after":"e1a5cf21701573b945572e85a117eed3a619e7dea97fc08f35835ac92d1dcaf2"}
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
-- ocservia:end-step
-- ocservia:step=006:table_business_locks
-- ocservia:metadata={"kind":"table","object":"business_locks","before":"","after":"9f4b1ecbaf1f861998bf97284c47c8374b8877585d5525785b462410bfc1900f"}
CREATE TABLE `business_locks` (
  `lock_key` varbinary(128) NOT NULL,
  PRIMARY KEY (`lock_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=007:table_connection_owner_fencing
-- ocservia:metadata={"kind":"table","object":"connection_owner_fencing","before":"","after":"30692d33c2b08b901a42308fdd06763f70f4d778e064d6a4d8384bbb0218dddb"}
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
-- ocservia:end-step
-- ocservia:step=008:table_controller_schema_compatibility
-- ocservia:metadata={"kind":"table","object":"controller_schema_compatibility","before":"","after":"f37ec951e686d057862e723ad6f9f19e867833dd9d6639b28b695ab0d0a7c11b"}
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
-- ocservia:end-step
-- ocservia:step=009:table_exact_key_guards
-- ocservia:metadata={"kind":"table","object":"exact_key_guards","before":"","after":"a8b4b4bcb8d3596b454120b367954fe86badf4604baf0cc78ad0c9947ef6e57c"}
CREATE TABLE `exact_key_guards` (
  `key_name` varbinary(64) NOT NULL,
  PRIMARY KEY (`key_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=010:table_identities
-- ocservia:metadata={"kind":"table","object":"identities","before":"","after":"bf83af509e1746e633c82cc77181cd6ce54232bf28e06883e4eaa613c20d797f"}
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
-- ocservia:end-step
-- ocservia:step=011:table_local_auth_attempts
-- ocservia:metadata={"kind":"table","object":"local_auth_attempts","before":"","after":"58101b0338e9d64c7bf1284b3a69470062d7dbec434dd8b434d5d07049fd9e79"}
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
-- ocservia:end-step
-- ocservia:step=012:table_local_credentials
-- ocservia:metadata={"kind":"table","object":"local_credentials","before":"","after":"8670731a44238f1770c3eb3099baf1edb7cfe01ec89a14f3b8b41108465fd475"}
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
-- ocservia:end-step
-- ocservia:step=013:table_node_agent_upgrade_results
-- ocservia:metadata={"kind":"table","object":"node_agent_upgrade_results","before":"","after":"6be15d3e187ab474e4bb0bb47c8860e84f587bd4cff631627129425186241a30"}
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
-- ocservia:end-step
-- ocservia:step=014:table_roles
-- ocservia:metadata={"kind":"table","object":"roles","before":"","after":"46eda1a93e1889c6cb1c5549f286c8a9da452fc1b2a7423fba78000e35e3b416"}
CREATE TABLE `roles` (
  `name` varchar(191) NOT NULL,
  PRIMARY KEY (`name`),
  CONSTRAINT `roles_roles_name_check` CHECK ((`name` in (_utf8mb4'Viewer',_utf8mb4'Operator',_utf8mb4'UserManager',_utf8mb4'ConfigManager',_utf8mb4'Auditor',_utf8mb4'SecurityAdmin',_utf8mb4'PlatformAdmin'))),
  CONSTRAINT `roles_text_no_nul` CHECK ((locate(0x00,cast(`name` as char charset binary)) = 0))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
-- ocservia:end-step
-- ocservia:step=015:table_scheduler_leadership
-- ocservia:metadata={"kind":"table","object":"scheduler_leadership","before":"","after":"9d9ee2ddc71be858788aa5b2f05ade840f69b35fd947514cf3513654201d12e3"}
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
-- ocservia:end-step
-- ocservia:step=016:table_scheduler_leases
-- ocservia:metadata={"kind":"table","object":"scheduler_leases","before":"","after":"8ba051041c577b040c52b3ad7b759529d33c88487c843fa4f6a2ff04b96a5054"}
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
-- ocservia:end-step
-- ocservia:step=017:table_schema_revisions
-- ocservia:metadata={"kind":"table","object":"schema_revisions","before":"","after":"e73ea739ba0b8d06f7cdc5e42a079c10cf168337b38bdc007a24a3cc36946c7e"}
CREATE TABLE `schema_revisions` (
  `epoch` bigint NOT NULL,
  `revision` bigint NOT NULL,
  `checksum` varbinary(64) NOT NULL,
  `state` varbinary(16) NOT NULL,
  `step` int NOT NULL,
  `started_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `verified_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`epoch`,`revision`),
  CONSTRAINT `schema_revisions_chk_1` CHECK ((`epoch` > 0)),
  CONSTRAINT `schema_revisions_chk_2` CHECK ((`revision` >= 0)),
  CONSTRAINT `schema_revisions_chk_3` CHECK ((length(`checksum`) = 64)),
  CONSTRAINT `schema_revisions_chk_4` CHECK ((`state` in (_utf8mb4'running',_utf8mb4'verified'))),
  CONSTRAINT `schema_revisions_chk_5` CHECK ((`step` >= 0)),
  CONSTRAINT `schema_revisions_chk_6` CHECK (((`state` = _utf8mb4'verified') = (`verified_at` is not null)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=018:table_telemetry_legacy_migration
-- ocservia:metadata={"kind":"table","object":"telemetry_legacy_migration","before":"","after":"48334549b788aca11bea0d68a8e9cc26ee4745168593f8f41f0cf5cc581d2f44"}
CREATE TABLE `telemetry_legacy_migration` (
  `singleton` tinyint NOT NULL,
  `state` varbinary(16) NOT NULL,
  `completed_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`singleton`),
  CONSTRAINT `telemetry_legacy_migration_chk_1` CHECK ((`singleton` = 1)),
  CONSTRAINT `telemetry_legacy_migration_chk_2` CHECK ((`state` in (_utf8mb4'pending',_utf8mb4'running',_utf8mb4'complete'))),
  CONSTRAINT `telemetry_legacy_migration_chk_3` CHECK (((`state` = _utf8mb4'complete') = (`completed_at` is not null)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=019:table_telemetry_maintenance_page
-- ocservia:metadata={"kind":"table","object":"telemetry_maintenance_page","before":"","after":"3ea4bde2c610d424e2e4a0a064a7bb84f89f8220875f06cbf0cbeaf32006140c"}
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
-- ocservia:end-step
-- ocservia:step=020:table_telemetry_maintenance_progress
-- ocservia:metadata={"kind":"table","object":"telemetry_maintenance_progress","before":"","after":"6cc9488596be597dd76df5f1ad84c308ed7fc1f2a32606bb9f8c7e9b6d33b994"}
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
-- ocservia:end-step
-- ocservia:step=021:table_telemetry_rollups_1h
-- ocservia:metadata={"kind":"table","object":"telemetry_rollups_1h","before":"","after":"e9abc6bf6141e4c86b564ec5e1fcbc1429cd86dc01696d0d231155f444db445f","roundtrip_hash":"6dbcfcd61373e1982859075d29608d5612cc6ba75a51599c7dc704dc8487c65e"}
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
-- ocservia:end-step
-- ocservia:step=022:table_telemetry_sample_shards
-- ocservia:metadata={"kind":"table","object":"telemetry_sample_shards","before":"","after":"27dc2880d60d71ba7543e4c111c076acad11be1ab37cf6bae5857364c0fbebc8"}
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
-- ocservia:end-step
-- ocservia:step=023:table_time_migration_decisions
-- ocservia:metadata={"kind":"table","object":"time_migration_decisions","before":"","after":"663062e47754eb195a4c88593c442fb820bd65dba72d834eb11c7e2e42e54c9a"}
CREATE TABLE `time_migration_decisions` (
  `table_name` varbinary(64) NOT NULL,
  `column_name` varbinary(64) NOT NULL,
  `row_key` varbinary(128) NOT NULL,
  `source_value` datetime(6) NOT NULL,
  `decision` varbinary(24) NOT NULL,
  PRIMARY KEY (`table_name`,`column_name`,`row_key`),
  CONSTRAINT `time_migration_decisions_chk_1` CHECK ((`decision` in (_utf8mb4'finite',_utf8mb4'negative_infinity')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=024:table_transport_event_cursor
-- ocservia:metadata={"kind":"table","object":"transport_event_cursor","before":"","after":"c8ef5ee545269871ea2f39b1ee8e9e0d4416fe32f71bf4c640b0a958f761726c"}
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
-- ocservia:end-step
-- ocservia:step=025:table_transport_event_quarantine
-- ocservia:metadata={"kind":"table","object":"transport_event_quarantine","before":"","after":"51c6f86d66c33455863bd1b50a746c031c2d719ac5d8674cbe948298f081bc75"}
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
-- ocservia:end-step
-- ocservia:step=026:table_upstream_sync_records
-- ocservia:metadata={"kind":"table","object":"upstream_sync_records","before":"","after":"a3b21deae18ebb0245bdf0795fa56cbbd99c59ed7d2cdaec8c683150efd1349f","roundtrip_hash":"e53323bc06134d05bee4e31671d2cc2bed69569cd68cf56df993298a0c0f0568"}
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
-- ocservia:end-step
-- ocservia:step=027:table_workspaces
-- ocservia:metadata={"kind":"table","object":"workspaces","before":"","after":"2ea509050fd1cffe93465a4db7de100b7055385004af3786779556a9f83f2000"}
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
-- ocservia:end-step
-- ocservia:step=028:table_approval_requests
-- ocservia:metadata={"kind":"table","object":"approval_requests","before":"","after":"97cb920a24d14e2c6a17281dbeb32610fb85348c6797ac8c5f8849f760442eea","roundtrip_hash":"4519bcd372033c2533eb2d92b858a7ed9d3287836ed4a85b243ae6be07e2ca5e"}
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
-- ocservia:end-step
-- ocservia:step=029:table_auth_sessions
-- ocservia:metadata={"kind":"table","object":"auth_sessions","before":"","after":"92b91700a9add79cdbe0dd0a646fd92fb996ac4d5c14a732ea52a8b699f9144e"}
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
-- ocservia:end-step
-- ocservia:step=030:table_backend_schema_revision_steps
-- ocservia:metadata={"kind":"table","object":"backend_schema_revision_steps","before":"","after":"2603a5a7427805e5e607905300e5eca6e81d96d0a424d5466fe3a899004b1006"}
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
-- ocservia:end-step
-- ocservia:step=031:table_batch_operations
-- ocservia:metadata={"kind":"table","object":"batch_operations","before":"","after":"e5e3644c5ac88a0ffcf698188d7f5f804b4ab23dc56ee5fb4c55b23dd8095210"}
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
-- ocservia:end-step
-- ocservia:step=032:table_break_glass_uses
-- ocservia:metadata={"kind":"table","object":"break_glass_uses","before":"","after":"ca9350e3dfa0ff2cfdd0499cf4d3607269b292308a85870ab15db25e9d524317"}
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
-- ocservia:end-step
-- ocservia:step=033:table_exact_identities
-- ocservia:metadata={"kind":"table","object":"exact_identities","before":"","after":"c2e78a8ab272853fc1f37c5e1296b824a1adb71f7d197f23fb643750f456a83c"}
CREATE TABLE `exact_identities` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_identities_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `identities` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=034:table_exact_telemetry_rollups_1h
-- ocservia:metadata={"kind":"table","object":"exact_telemetry_rollups_1h","before":"","after":"5af99d4d767e759aaea8baf5470f8d862183f984b998e9fff3d63c62b16cc607"}
CREATE TABLE `exact_telemetry_rollups_1h` (
  `owner_id` bigint unsigned NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  KEY `telemetry_key_lookup` (`key_value`(255)),
  CONSTRAINT `exact_telemetry_rollups_1h_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `telemetry_rollups_1h` (`exact_row_id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=035:table_exact_upstream_sync_records
-- ocservia:metadata={"kind":"table","object":"exact_upstream_sync_records","before":"","after":"1573a172c531ed437db21e54497fecc0f542e28bc845b4ec135c8242b4d383f0"}
CREATE TABLE `exact_upstream_sync_records` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_upstream_sync_records_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `upstream_sync_records` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=036:table_exact_workspaces
-- ocservia:metadata={"kind":"table","object":"exact_workspaces","before":"","after":"8695c9cad3a5deabbcc8ec598f522e0b9412a7412dbb2e624299a24ed70322a6"}
CREATE TABLE `exact_workspaces` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_workspaces_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=037:table_local_auth_bootstrap
-- ocservia:metadata={"kind":"table","object":"local_auth_bootstrap","before":"","after":"4e68075758be755f595527045e1c6f847c18ed090535d7cfa959296260dd395a","roundtrip_hash":"b9d6416b16c4fadd52b818a5d016214bcefaf68b954ed0682bbaf65c9fa613e8"}
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
-- ocservia:end-step
-- ocservia:step=038:table_nodes
-- ocservia:metadata={"kind":"table","object":"nodes","before":"","after":"7a1f5c2a73e1c9861c4eb7b2103fda8d3909d76b2a73c3816adba1085a3f6c0a","roundtrip_hash":"8d2fa654e02438a5cc460125a9d3daa5f8b9b7c23da90867a7c4bd2e40bd90ef"}
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
-- ocservia:end-step
-- ocservia:step=039:table_observed_groups
-- ocservia:metadata={"kind":"table","object":"observed_groups","before":"","after":"b3c272fc8e6909b19e2c2db88be3a25a1f16226dc4d27dd37c925379cf035311","roundtrip_hash":"a4bf80cfa9c097365a6bc8466109ef3ddfdb889d2a80c41be3b03fd51f7da260"}
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
-- ocservia:end-step
-- ocservia:step=040:table_observed_user_usage
-- ocservia:metadata={"kind":"table","object":"observed_user_usage","before":"","after":"80ee82e04030bb09e229798aa572622d4b44d45e901de1890f0c5e5249ae89ec"}
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
-- ocservia:end-step
-- ocservia:step=041:table_observed_users
-- ocservia:metadata={"kind":"table","object":"observed_users","before":"","after":"cee27d1272f65fff7e8b4b04ce158aecb4b9792388f0733e0e5f9a651aacd649"}
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
-- ocservia:end-step
-- ocservia:step=042:table_operations
-- ocservia:metadata={"kind":"table","object":"operations","before":"","after":"717478c2f814542e2a8222f2faabf3fc5dca56166e49fad6a778746698f681de"}
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
-- ocservia:end-step
-- ocservia:step=043:table_privd_attestation_enrollment_credentials
-- ocservia:metadata={"kind":"table","object":"privd_attestation_enrollment_credentials","before":"","after":"4daa6f628fa82f7226ab31e0549cf44dea9ddec7276e2e64074534c65e442092"}
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
-- ocservia:end-step
-- ocservia:step=044:table_role_bindings
-- ocservia:metadata={"kind":"table","object":"role_bindings","before":"","after":"5b6e28e30545deb80973ac1718625e3d0737620ed7a3e3d1d53a400a9b82f774"}
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
-- ocservia:end-step
-- ocservia:step=045:table_secret_provider_refs
-- ocservia:metadata={"kind":"table","object":"secret_provider_refs","before":"","after":"7fd70a1d159104967a304d613586a47329e1d68ff7bb00dee61b87e5bade43f8"}
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
-- ocservia:end-step
-- ocservia:step=046:table_security_alerts
-- ocservia:metadata={"kind":"table","object":"security_alerts","before":"","after":"7df3fe7699d02056cb01f6b69a209f6421efc07704e8bb1fdbd016a394ae8d96"}
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
-- ocservia:end-step
-- ocservia:step=047:table_telemetry_ingest_batches
-- ocservia:metadata={"kind":"table","object":"telemetry_ingest_batches","before":"","after":"8d4281b0cb1bee5d7d3e0d1d504b9c4ea9cefe76a629accfc15933f1d0266ae6"}
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
-- ocservia:end-step
-- ocservia:step=048:table_telemetry_rollups_5m
-- ocservia:metadata={"kind":"table","object":"telemetry_rollups_5m","before":"","after":"9eca8abde6bd5f0802c013197210f824c69fa9ffb499e044ecc57d9f1e9db6e6","roundtrip_hash":"2dd28c5da12e83ed3a62410a933c7885ceca9a26083bb98cbc11b812ce0d4623"}
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
-- ocservia:end-step
-- ocservia:step=049:table_telemetry_samples
-- ocservia:metadata={"kind":"table","object":"telemetry_samples","before":"","after":"8817627c7dc1a884b144887d66195217095ffbfee371bf041c068f4e7e88e0bf"}
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
-- ocservia:end-step
-- ocservia:step=050:table_telemetry_samples_template
-- ocservia:metadata={"kind":"table","object":"telemetry_samples_template","before":"","after":"f3fb8735314a026419596c51cee839f2fc29939faa0a968662082d538b3d2afb"}
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
-- ocservia:end-step
-- ocservia:step=051:table_telemetry_security_events
-- ocservia:metadata={"kind":"table","object":"telemetry_security_events","before":"","after":"53b330f15111a22f197885bce02d9de5c55cd3f1bec5c0a2a01643018554c0c6","roundtrip_hash":"a02b97a9949a2d511e9efac94dcb0f37e07cad850e242e409911d8d65815b20b"}
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
-- ocservia:end-step
-- ocservia:step=052:table_transport_events
-- ocservia:metadata={"kind":"table","object":"transport_events","before":"","after":"ea32f66593b14c52b175bac0e7b17ff1a0f0547a4569c5179356351f6233e428"}
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
-- ocservia:end-step
-- ocservia:step=053:table_user_policy_enforcements
-- ocservia:metadata={"kind":"table","object":"user_policy_enforcements","before":"","after":"e5039598d5df4f2973bcb50372601dc34be5c716bd8e7a0060f35410627d109b"}
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
-- ocservia:end-step
-- ocservia:step=054:table_user_usage_cursors
-- ocservia:metadata={"kind":"table","object":"user_usage_cursors","before":"","after":"2293a3c32b56cf00a7227fc0488b919a4d2a6c4d94314faf8337a88bd51b51c6"}
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
-- ocservia:end-step
-- ocservia:step=055:table_agent_rollouts
-- ocservia:metadata={"kind":"table","object":"agent_rollouts","before":"","after":"a84081f92b5188606851515077b5fa341d31a91b27aee1d2fea769e0218f0283","roundtrip_hash":"2aaaba9ac76e6da8dde959b7192014bd78e900dff5f638c7adcfa21723988e7b"}
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
-- ocservia:end-step
-- ocservia:step=056:table_agent_upgrade_operations
-- ocservia:metadata={"kind":"table","object":"agent_upgrade_operations","before":"","after":"c41e661c81c7c337c4ebe64fc212cb4c74c29e534c3824e61937f2a2c3d28cb3"}
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
-- ocservia:end-step
-- ocservia:step=057:table_approval_authority_resources
-- ocservia:metadata={"kind":"table","object":"approval_authority_resources","before":"","after":"c0ab6709f2572097b7325212f5159ae817940ab0a97ee78e4ceb34d28c77b77b"}
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
-- ocservia:end-step
-- ocservia:step=058:table_approval_batch_items
-- ocservia:metadata={"kind":"table","object":"approval_batch_items","before":"","after":"f1ed94dd72a7c8a9f9f708ee811a2a3d9eb93c0ae63a05c047cc354ac336392f"}
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
-- ocservia:end-step
-- ocservia:step=059:table_audit_events
-- ocservia:metadata={"kind":"table","object":"audit_events","before":"","after":"e906eaf8253d146c5e6da7fc520f27829a22859fe53349f4a31dbed609df28ec","roundtrip_hash":"8a9c49f8b7aaa44baf2507e3ab50f6a5c7ca92fa21fc4235ea12489fa414a1e3"}
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
-- ocservia:end-step
-- ocservia:step=060:table_batch_operation_items
-- ocservia:metadata={"kind":"table","object":"batch_operation_items","before":"","after":"d0670bb300329f5f9d47703c155945cfb6d8819f10664f6ac1ec52ea0c8fe260"}
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
-- ocservia:end-step
-- ocservia:step=061:table_commands
-- ocservia:metadata={"kind":"table","object":"commands","before":"","after":"f410bffa77d37187d35d45b7908839f4504d713ec0ed17f8f203459d0bf32566","roundtrip_hash":"879e06d964192008e57e8e56e7bb348bd22ddae7a1dcfb4294fd381970ed8be7"}
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
-- ocservia:end-step
-- ocservia:step=062:table_config_plans
-- ocservia:metadata={"kind":"table","object":"config_plans","before":"","after":"b7161a297d8ac719cffcf41821a66c67bc3db122046712be0c514e4fee77c3b0","roundtrip_hash":"4aac23b76d532452223d6afa9d659db5605b96ef2a411a69b869224687d0c5a7"}
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
-- ocservia:end-step
-- ocservia:step=063:table_desired_groups
-- ocservia:metadata={"kind":"table","object":"desired_groups","before":"","after":"a4bbfdc9c5a9fb66dc034bbc549ec0e797e91a2e51012b7aef5247b7d39335c9","roundtrip_hash":"f932d324cc915d5f94a3f39ce4d31e3549ab22e5a49b3d138829a27ace72b487"}
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
-- ocservia:end-step
-- ocservia:step=064:table_desired_users
-- ocservia:metadata={"kind":"table","object":"desired_users","before":"","after":"65e18b911849a826a6f493ff7d9f25f674c8165c90f3ff9037f0654e5b013f21"}
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
-- ocservia:end-step
-- ocservia:step=065:table_enrollment_tokens
-- ocservia:metadata={"kind":"table","object":"enrollment_tokens","before":"","after":"0f0df8b1f145519d3a59969bf1de2139a6fb378080cda02e77a2f4caee39edf4"}
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
-- ocservia:end-step
-- ocservia:step=066:table_exact_nodes
-- ocservia:metadata={"kind":"table","object":"exact_nodes","before":"","after":"68702bd738eccf645e8c0612c44524b86f861562d1632f20885558667bd583c7"}
CREATE TABLE `exact_nodes` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_nodes_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=067:table_exact_operations
-- ocservia:metadata={"kind":"table","object":"exact_operations","before":"","after":"02846fdea0d1956598f674ff40560e057ffce44ea9e27617fe348e72bbd6d3cb"}
CREATE TABLE `exact_operations` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_operations_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `operations` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=068:table_exact_telemetry_rollups_5m
-- ocservia:metadata={"kind":"table","object":"exact_telemetry_rollups_5m","before":"","after":"0471497d9e8898a0672402ee6d3a5185f39f79760deb60b2c55cd179cb61db45"}
CREATE TABLE `exact_telemetry_rollups_5m` (
  `owner_id` bigint unsigned NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  KEY `telemetry_key_lookup` (`key_value`(255)),
  CONSTRAINT `exact_telemetry_rollups_5m_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `telemetry_rollups_5m` (`exact_row_id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=069:table_exact_user_policy_enforcements
-- ocservia:metadata={"kind":"table","object":"exact_user_policy_enforcements","before":"","after":"06c69b88b9e5fded295262c25ddc6f073ddc32d4a005b3a8f73181152daf5cbd"}
CREATE TABLE `exact_user_policy_enforcements` (
  `owner_id` bigint unsigned NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_user_policy_enforcements_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `user_policy_enforcements` (`exact_row_id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=070:table_local_slice_jobs
-- ocservia:metadata={"kind":"table","object":"local_slice_jobs","before":"","after":"bdf301ebf13b3d3765286f53e5fce482ca2175821c987b4ec1f9468f645d4996"}
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
-- ocservia:end-step
-- ocservia:step=071:table_node_bootstrap_tokens
-- ocservia:metadata={"kind":"table","object":"node_bootstrap_tokens","before":"","after":"dfadc4846ced8dc13712540de26ed4f4e3cabe9557f263cf4e30e1a5f1bacb67","roundtrip_hash":"fdccc8c94507bf25c67462debe329409ddc7147fd7b35db115fc0e0a092c24b4"}
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
-- ocservia:end-step
-- ocservia:step=072:table_node_capabilities
-- ocservia:metadata={"kind":"table","object":"node_capabilities","before":"","after":"662913f77ecb54d1e749cdf65d6d4f13353fd3418b368387dc2b1c8d7f4e309e"}
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
-- ocservia:end-step
-- ocservia:step=073:table_node_command_leases
-- ocservia:metadata={"kind":"table","object":"node_command_leases","before":"","after":"ee44f894764e98467eb79767e9c625399c7880f1b53a898766e5208e909d0b02"}
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
-- ocservia:end-step
-- ocservia:step=074:table_node_config_state
-- ocservia:metadata={"kind":"table","object":"node_config_state","before":"","after":"ab117e3e63eb979858073c8b46db5137a5a4546aff2966b5453dd7aac6a914e3"}
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
-- ocservia:end-step
-- ocservia:step=075:table_node_endpoint_keys
-- ocservia:metadata={"kind":"table","object":"node_endpoint_keys","before":"","after":"b20e4515625196371230f248eadae743fac842dd66cab2e0219da82b46bbc9ef"}
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
-- ocservia:end-step
-- ocservia:step=076:table_node_ip_bans
-- ocservia:metadata={"kind":"table","object":"node_ip_bans","before":"","after":"97d1c8eba5d757bc327ea6049c2f9e2b193bfd6babc27286173fc1a86ce95a60"}
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
-- ocservia:end-step
-- ocservia:step=077:table_node_observed_snapshots
-- ocservia:metadata={"kind":"table","object":"node_observed_snapshots","before":"","after":"848aa902295ab4394e587be5876e4c447643c8b270144a6f637aeb7ed3c56e53","roundtrip_hash":"cc8e0a64949b83bedba0c9e44ae98ac6a2becabd70fe1fb79368d061930a8e5f"}
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
-- ocservia:end-step
-- ocservia:step=078:table_node_privd_attestation_keys
-- ocservia:metadata={"kind":"table","object":"node_privd_attestation_keys","before":"","after":"b28c2c04dd1315cf41bdc7d5742ed694d784580dcecb06b1aacfd6914418d241"}
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
-- ocservia:end-step
-- ocservia:step=079:table_node_sealing_keys
-- ocservia:metadata={"kind":"table","object":"node_sealing_keys","before":"","after":"fe2351eadf1e505b142b2c49989c9307b74369d45af5ddc8cd8f45dea50d8875"}
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
-- ocservia:end-step
-- ocservia:step=080:table_node_sessions
-- ocservia:metadata={"kind":"table","object":"node_sessions","before":"","after":"459c39e773a1144757b4072f58079d3360471ee83db8df5789a1e730937f90b7"}
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
-- ocservia:end-step
-- ocservia:step=081:table_node_trust_convergence
-- ocservia:metadata={"kind":"table","object":"node_trust_convergence","before":"","after":"34e64d48eff98ca1b89f32b1412c7fa4672d1ac644681bc6013c99719fc64c38"}
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
-- ocservia:end-step
-- ocservia:step=082:table_operation_events
-- ocservia:metadata={"kind":"table","object":"operation_events","before":"","after":"0d4a03e496adca5eb5a889443c8628e2c99493f1dd38322b86c6bf05ee58301c"}
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
-- ocservia:end-step
-- ocservia:step=083:table_outbox_events
-- ocservia:metadata={"kind":"table","object":"outbox_events","before":"","after":"7cde03686e128108f572d7d2a6c907d10b4c5ca2740e1dd8e791d6c1a25eac61"}
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
-- ocservia:end-step
-- ocservia:step=084:table_agent_command_results
-- ocservia:metadata={"kind":"table","object":"agent_command_results","before":"","after":"d9a6f6c2b17e0df20db2e26919b2bf474cc4399313e74ea5fc5c657a8d74cc84"}
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
-- ocservia:end-step
-- ocservia:step=085:table_agent_rollout_nodes
-- ocservia:metadata={"kind":"table","object":"agent_rollout_nodes","before":"","after":"3acb2724008a862c40722be5792343ae04df36dc574dd6131b286d78e1ae6eb8"}
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
-- ocservia:end-step
-- ocservia:step=086:table_audit_checkpoints
-- ocservia:metadata={"kind":"table","object":"audit_checkpoints","before":"","after":"fa13bb11bfde738c82cf23be49b0774e1da838cfacff7e66824efbcac3939074"}
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
-- ocservia:end-step
-- ocservia:step=087:table_certificates
-- ocservia:metadata={"kind":"table","object":"certificates","before":"","after":"10c5abbd88daceaab3745ea426bacdda647365bbec7763f55683244346d60989","roundtrip_hash":"af5ebec27b320eb68e64390e7f6a7091ece15b5847478a6e308aab40c90ccfc4"}
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
-- ocservia:end-step
-- ocservia:step=088:table_command_attempts
-- ocservia:metadata={"kind":"table","object":"command_attempts","before":"","after":"a1e4490345fc773979b679b52dc78f9c0045c55867b220b4f0e9e14907479109"}
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
-- ocservia:end-step
-- ocservia:step=089:table_config_apply_operations
-- ocservia:metadata={"kind":"table","object":"config_apply_operations","before":"","after":"a154f572a0991d968c7ddc671372d6cba272c0ac40c6af5d1e0e7c6d2d3b577a"}
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
-- ocservia:end-step
-- ocservia:step=090:table_desired_user_policies
-- ocservia:metadata={"kind":"table","object":"desired_user_policies","before":"","after":"a8e28b2d29b094de0a9b9418b8c0f6596bd274e923b8a1c2b2aba3a60bf0304b"}
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
-- ocservia:end-step
-- ocservia:step=091:table_exact_agent_command_results
-- ocservia:metadata={"kind":"table","object":"exact_agent_command_results","before":"","after":"35974c8aeea59286c7114b5be210f04a49456c1a8ace600d9394827fbd4175db"}
CREATE TABLE `exact_agent_command_results` (
  `owner_id` varbinary(16) NOT NULL,
  `key_value` longblob NOT NULL,
  PRIMARY KEY (`owner_id`),
  CONSTRAINT `exact_agent_command_results_owner_fk` FOREIGN KEY (`owner_id`) REFERENCES `agent_command_results` (`event_id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- ocservia:end-step
-- ocservia:step=092:table_user_policy_mutations
-- ocservia:metadata={"kind":"table","object":"user_policy_mutations","before":"","after":"b94a8c5fb71a5202f89dc9f9f1797ea51afa81abc33cbb16220148c6b19dac3b"}
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
-- ocservia:end-step
-- ocservia:step=093:table_artifact_operations
-- ocservia:metadata={"kind":"table","object":"artifact_operations","before":"","after":"9e7b61b011559eaf747cc662327a3690aaf8fa6cd26144bceabc5a75b7015222"}
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
-- ocservia:end-step
-- ocservia:step=094:seed_business_locks
-- ocservia:metadata={"kind":"seed","object":"business_locks","before":"","after":"92aaf496e8ae19e8796e43a3329e252b37aacbdb9de474ce84713a30c0061042","columns":["lock_key"]}
INSERT INTO `business_locks` (`lock_key`) VALUES (CAST(X'74656c656d657472792d73686172642d636174616c6f67' AS BINARY));
-- ocservia:end-step
-- ocservia:step=095:seed_exact_key_guards
-- ocservia:metadata={"kind":"seed","object":"exact_key_guards","before":"","after":"c88a67659c491569d1c10745f32bedd9a683bad6a516639162aa1251fa427c39","columns":["key_name"]}
INSERT INTO `exact_key_guards` (`key_name`) VALUES (CAST(X'6167656e745f636f6d6d616e645f726573756c7473' AS BINARY)),(CAST(X'6964656e746974696573' AS BINARY)),(CAST(X'6e6f646573' AS BINARY)),(CAST(X'6f7065726174696f6e73' AS BINARY)),(CAST(X'74656c656d657472795f726f6c6c7570735f3168' AS BINARY)),(CAST(X'74656c656d657472795f726f6c6c7570735f356d' AS BINARY)),(CAST(X'757073747265616d5f73796e635f7265636f726473' AS BINARY)),(CAST(X'757365725f706f6c6963795f656e666f7263656d656e7473' AS BINARY)),(CAST(X'776f726b737061636573' AS BINARY));
-- ocservia:end-step
-- ocservia:step=096:seed_roles
-- ocservia:metadata={"kind":"seed","object":"roles","before":"","after":"1f9d1718f823ed7d16a33c4dc97cfb968d081b9809f81460d76988f19da3168e","columns":["name"]}
INSERT INTO `roles` (`name`) VALUES (CAST(X'41756469746f72' AS BINARY)),(CAST(X'436f6e6669674d616e61676572' AS BINARY)),(CAST(X'4f70657261746f72' AS BINARY)),(CAST(X'506c6174666f726d41646d696e' AS BINARY)),(CAST(X'536563757269747941646d696e' AS BINARY)),(CAST(X'557365724d616e61676572' AS BINARY)),(CAST(X'566965776572' AS BINARY));
-- ocservia:end-step
-- ocservia:step=097:seed_scheduler_leadership
-- ocservia:metadata={"kind":"seed","object":"scheduler_leadership","before":"","after":"998312bbfe219bdf8797c58c6497c55682ed0ef451927d5d054175f27bbdcfe5","columns":["id","instance_id","incarnation","epoch","lease_until"]}
INSERT INTO `scheduler_leadership` (`id`,`instance_id`,`incarnation`,`epoch`,`lease_until`,`updated_at`) VALUES (CAST(X'31' AS BINARY),CAST(X'00000000000000000000000000000000' AS BINARY),CAST(X'30' AS BINARY),CAST(X'30' AS BINARY),CAST(X'2d39323233333732303336383534373735383038' AS BINARY),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)));
-- ocservia:end-step
-- ocservia:step=098:seed_telemetry_legacy_migration
-- ocservia:metadata={"kind":"seed","object":"telemetry_legacy_migration","before":"","after":"afbdec78e9cf5ec42f10219b9985277a9ac666bc61c561ae4f5e078a18368d7f","columns":["singleton","state","completed_at"]}
INSERT INTO `telemetry_legacy_migration` (`singleton`,`state`,`completed_at`) VALUES (CAST(X'31' AS BINARY),CAST(X'70656e64696e67' AS BINARY),NULL);
-- ocservia:end-step
-- ocservia:step=099:seed_telemetry_maintenance_progress
-- ocservia:metadata={"kind":"seed","object":"telemetry_maintenance_progress","before":"","after":"f1581c3a8fa55c72b787832eaa9b6c7022fb113dda6510ca3daf3ee632dd6b04","columns":["singleton","phase","cutoff","candidate","cursor_at","cursor_node","cursor_metric"]}
INSERT INTO `telemetry_maintenance_progress` (`singleton`,`phase`,`cutoff`,`candidate`,`cursor_at`,`cursor_node`,`cursor_metric`) VALUES (CAST(X'31' AS BINARY),CAST(X'30' AS BINARY),CAST(X'30' AS BINARY),NULL,NULL,NULL,NULL);
-- ocservia:end-step
-- ocservia:step=100:seed_upstream_sync_records
-- ocservia:metadata={"kind":"seed","object":"upstream_sync_records","before":"","after":"e829ca9f651d9a71f9a8653a1b5bde893c992bcd691f31c10bb375e9d2a783f4","columns":["id","repository","old_ref","old_commit","new_ref","new_commit","rollback_ref","synced_at","classification"]}
INSERT INTO `upstream_sync_records` (`id`,`repository`,`old_ref`,`old_commit`,`new_ref`,`new_commit`,`rollback_ref`,`synced_at`,`classification`) VALUES (CAST(X'019fdc5bb93972a1ae678efd197e5688' AS BINARY),CAST(X'6d6d746165652f6f63736572762d64617368626f617264' AS BINARY),CAST(X'76342e39' AS BINARY),CAST(X'62386635393032366334643837396634306331646134336463303064393765333466393739306263' AS BINARY),CAST(X'6d6173746572' AS BINARY),CAST(X'34643235343738353830643839396237373436306264663063663061353930636664643236303330' AS BINARY),CAST(X'7075626c69636174696f6e3a20726576657274205052313520696e646570656e64656e746c793b20696d706c656d656e746174696f6e3a2073746f7020493134207363686564756c65722f4150492c207265636f6e63696c6520636f6d6d616e64732c2072657665727420505231342c207468656e206170706c79206d6967726174696f6e2030303030313320646f776e206f6e6c79207768656e20706f6c69637920616e642062617463682064617461206e656564206e6f742062652072657461696e6564' AS BINARY),CAST(X'383339343336313132303030303030' AS BINARY),CAST(X'7b2241223a205b5d2c202242223a205b227765622f7372632f636f6d706f6e656e74732f617574682f5365747570466f726d2e767565225d2c202243223a205b2271756f746120616e64206578706972792073656d616e74696373206d617070656420746f206e6f64652d73636f706564206465736972656420706f6c69637920616e64207363686564756c6572225d2c202244223a205b22446f636b65722f6e6174697665206f6363746c20657865637574696f6e222c20226c6f63616c2063726f6e206a6f75726e616c222c20226469726563742070617373776f72642f636f6e6669672066696c6573222c20227065726d616e656e742064656c6574696f6e225d7d' AS BINARY));
-- ocservia:end-step
-- ocservia:step=101:seed_exact_upstream_sync_records
-- ocservia:metadata={"kind":"seed","object":"exact_upstream_sync_records","before":"","after":"02816ec964df1e6f17fc63b66da17ed98f7206b02f4bfd2cdef0ae3d72db2426","columns":["owner_id","key_value"]}
INSERT INTO `exact_upstream_sync_records` (`owner_id`,`key_value`) VALUES (CAST(X'019fdc5bb93972a1ae678efd197e5688' AS BINARY),CAST(X'00000000000000176d6d746165652f6f63736572762d64617368626f617264000000000000002862386635393032366334643837396634306331646134336463303064393765333466393739306263000000000000002834643235343738353830643839396237373436306264663063663061353930636664643236303330' AS BINARY));
-- ocservia:end-step
-- ocservia:step=102:function_ocserv_jsonb_array_valid
-- ocservia:metadata={"kind":"function","object":"ocserv_jsonb_array_valid","before":"","after":"243f01f66c11a70d31c91e1c43de9bcb109decdb2d7e03c3ce874e63c094ce20"}
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
-- ocservia:end-step
-- ocservia:step=103:function_ocserv_jsonb_object_valid
-- ocservia:metadata={"kind":"function","object":"ocserv_jsonb_object_valid","before":"","after":"40cc96bbe056f9c8cb9b649d08f02f2f9c5b0d810c440c3173d9a961cc3de6bf"}
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
-- ocservia:end-step
-- ocservia:step=104:function_ocserv_jsonb_value_valid
-- ocservia:metadata={"kind":"function","object":"ocserv_jsonb_value_valid","before":"","after":"4aacb7b9f59e94989a011dbc355c05f13f15100af9ee4ac5d0ddda98ce8f4d01"}
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
-- ocservia:end-step
-- ocservia:step=105:function_ocserv_rollout_exclusions_valid
-- ocservia:metadata={"kind":"function","object":"ocserv_rollout_exclusions_valid","before":"","after":"a9b904951431aba59bf35fd0073e5f41b44c4fd1176ab45d870aca7009c2208e"}
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
-- ocservia:end-step
-- ocservia:step=106:function_ocserv_text_array_valid
-- ocservia:metadata={"kind":"function","object":"ocserv_text_array_valid","before":"","after":"05fbc9dd60753f3c42d99d1491c1a1236dd54f14ba8b229e8b9bd6a9c9a05092"}
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
-- ocservia:end-step
-- ocservia:step=107:procedure_audit_compact_detail
-- ocservia:metadata={"kind":"procedure","object":"audit_compact_detail","before":"","after":"4b15b21344c4e118d13e5a58743d17c7a416efa70b4576d466c6fe39f09e3cc8"}
CREATE PROCEDURE `audit_compact_detail`(IN p_id VARBINARY(16),IN p_hash VARBINARY(32),IN p_key VARBINARY(128),IN p_mac VARBINARY(32),IN p_at BIGINT)
BEGIN UPDATE audit_events SET reason=NULL,before_summary=NULL,after_summary=NULL,details_compacted_at=p_at,compaction_key_id=p_key,compaction_mac=p_mac WHERE id=p_id AND event_hash=p_hash AND details_compacted_at IS NULL AND auth_version=1 AND BINARY action<>BINARY 'audit.auth.transition' AND occurred_at<=p_at-7776000000000 AND p_at<=CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED); SELECT ROW_COUNT()=1; END;
-- ocservia:end-step
-- ocservia:step=108:procedure_security_compact_details
-- ocservia:metadata={"kind":"procedure","object":"security_compact_details","before":"","after":"87a9648de7e89b83a9ab3287a2ef22d67a667a7760d54acee75933488cd07035"}
CREATE PROCEDURE `security_compact_details`(IN p_cutoff BIGINT)
BEGIN UPDATE telemetry_security_events SET detail_sha256=UNHEX(SHA2(detail,256)),detail='{}',details_compacted_at=CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED) WHERE details_compacted_at IS NULL AND observed_at<LEAST(p_cutoff,CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED)-7776000000000) ORDER BY observed_at,event_id LIMIT 32; END;
-- ocservia:end-step
-- ocservia:step=109:procedure_telemetry_prune_rollups
-- ocservia:metadata={"kind":"procedure","object":"telemetry_prune_rollups","before":"","after":"32318aacdcf8bab5f5ad87689f14f72a3f2109933e6c0420ab6c9671656d8e0d"}
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
-- ocservia:end-step
-- ocservia:step=110:procedure_telemetry_retire_shards
-- ocservia:metadata={"kind":"procedure","object":"telemetry_retire_shards","before":"","after":"9cd38b1d7f8dd965de77176f3c5c60197ff57e027b8fbe2c2403df09524ee9a5"}
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
-- ocservia:end-step
-- ocservia:step=111:trigger_approval_summary_insert
-- ocservia:metadata={"kind":"trigger","object":"approval_summary_insert","before":"","after":"22dbb092adf61e04ada2af2d43a57e7cfbca12ba8e15cc3676a3cb6aaa5f8869"}
CREATE TRIGGER `approval_summary_insert` BEFORE INSERT ON `approval_requests` FOR EACH ROW BEGIN IF NEW.request_summary IS NOT NULL AND (NOT ocserv_jsonb_value_valid(NEW.request_summary) OR LEFT(TRIM(REPLACE(REPLACE(REPLACE(CONVERT(NEW.request_summary USING utf8mb4),CHAR(9),' '),CHAR(10),' '),CHAR(13),' ')),1) NOT IN ('[','{')) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid approval JSON'; END IF; END;
-- ocservia:end-step
-- ocservia:step=112:trigger_approval_summary_update
-- ocservia:metadata={"kind":"trigger","object":"approval_summary_update","before":"","after":"93ad0f62439a19fd2257ac1115f28279a6b2e5a07ac86993d49ca26a593856c3"}
CREATE TRIGGER `approval_summary_update` BEFORE UPDATE ON `approval_requests` FOR EACH ROW BEGIN IF NEW.request_summary IS NOT NULL AND (NOT ocserv_jsonb_value_valid(NEW.request_summary) OR LEFT(TRIM(REPLACE(REPLACE(REPLACE(CONVERT(NEW.request_summary USING utf8mb4),CHAR(9),' '),CHAR(10),' '),CHAR(13),' ')),1) NOT IN ('[','{')) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid approval JSON'; END IF; END;
-- ocservia:end-step
-- ocservia:step=113:trigger_audit_checkpoints_reject_delete
-- ocservia:metadata={"kind":"trigger","object":"audit_checkpoints_reject_delete","before":"","after":"0af7c06405d4177292543f7960aee5fa35b0ff0cad29542b6f3488d68af69084"}
CREATE TRIGGER `audit_checkpoints_reject_delete` BEFORE DELETE ON `audit_checkpoints` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='append-only audit record';
-- ocservia:end-step
-- ocservia:step=114:trigger_audit_checkpoints_reject_update
-- ocservia:metadata={"kind":"trigger","object":"audit_checkpoints_reject_update","before":"","after":"fa482fd81b40e17cd98fb23c88a3c0fe93400cd99555a6dbe22182ab25f7d923"}
CREATE TRIGGER `audit_checkpoints_reject_update` BEFORE UPDATE ON `audit_checkpoints` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='append-only audit record';
-- ocservia:end-step
-- ocservia:step=115:trigger_audit_events_reject_delete
-- ocservia:metadata={"kind":"trigger","object":"audit_events_reject_delete","before":"","after":"c840e13d3104c6d8a3deb37c080818af932691fb655ff5f066fd5ad9545fd0bd"}
CREATE TRIGGER `audit_events_reject_delete` BEFORE DELETE ON `audit_events` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='append-only audit record';
-- ocservia:end-step
-- ocservia:step=116:trigger_audit_events_reject_update
-- ocservia:metadata={"kind":"trigger","object":"audit_events_reject_update","before":"","after":"ba2087afbd4d5fdeffeabade078bd579b78df879f939345e33199d5cfe6817fb"}
CREATE TRIGGER `audit_events_reject_update` BEFORE UPDATE ON `audit_events` FOR EACH ROW BEGIN IF NOT (
 OLD.details_compacted_at IS NULL AND OLD.auth_version=1 AND BINARY OLD.action<>BINARY 'audit.auth.transition'
 AND NEW.details_compacted_at IS NOT NULL AND OLD.occurred_at<=NEW.details_compacted_at-7776000000000
 AND NEW.details_compacted_at<=CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED)
 AND NEW.reason IS NULL AND NEW.before_summary IS NULL AND NEW.after_summary IS NULL
 AND (CAST(NEW.`id` AS BINARY)<=>CAST(OLD.`id` AS BINARY)) AND (CAST(NEW.`workspace_id` AS BINARY)<=>CAST(OLD.`workspace_id` AS BINARY)) AND (CAST(NEW.`occurred_at` AS BINARY)<=>CAST(OLD.`occurred_at` AS BINARY)) AND (CAST(NEW.`actor_type` AS BINARY)<=>CAST(OLD.`actor_type` AS BINARY)) AND (CAST(NEW.`actor_id` AS BINARY)<=>CAST(OLD.`actor_id` AS BINARY)) AND (CAST(NEW.`source_session_id` AS BINARY)<=>CAST(OLD.`source_session_id` AS BINARY)) AND (CAST(NEW.`action` AS BINARY)<=>CAST(OLD.`action` AS BINARY)) AND (CAST(NEW.`resource_type` AS BINARY)<=>CAST(OLD.`resource_type` AS BINARY)) AND (CAST(NEW.`resource_id` AS BINARY)<=>CAST(OLD.`resource_id` AS BINARY)) AND (CAST(NEW.`node_id` AS BINARY)<=>CAST(OLD.`node_id` AS BINARY)) AND (CAST(NEW.`request_id` AS BINARY)<=>CAST(OLD.`request_id` AS BINARY)) AND (CAST(NEW.`trace_id` AS BINARY)<=>CAST(OLD.`trace_id` AS BINARY)) AND (CAST(NEW.`command_id` AS BINARY)<=>CAST(OLD.`command_id` AS BINARY)) AND (CAST(NEW.`approval_id` AS BINARY)<=>CAST(OLD.`approval_id` AS BINARY)) AND (CAST(NEW.`result` AS BINARY)<=>CAST(OLD.`result` AS BINARY)) AND (CAST(NEW.`error_type` AS BINARY)<=>CAST(OLD.`error_type` AS BINARY)) AND (CAST(NEW.`previous_event_hash` AS BINARY)<=>CAST(OLD.`previous_event_hash` AS BINARY)) AND (CAST(NEW.`event_hash` AS BINARY)<=>CAST(OLD.`event_hash` AS BINARY)) AND (CAST(NEW.`auth_version` AS BINARY)<=>CAST(OLD.`auth_version` AS BINARY)) AND (CAST(NEW.`event_key_id` AS BINARY)<=>CAST(OLD.`event_key_id` AS BINARY)) AND (CAST(NEW.`event_mac` AS BINARY)<=>CAST(OLD.`event_mac` AS BINARY))) THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='audit permits only authenticated aged detail compaction'; END IF; END;
-- ocservia:end-step
-- ocservia:step=117:trigger_audit_jsonb_insert
-- ocservia:metadata={"kind":"trigger","object":"audit_jsonb_insert","before":"","after":"3d191970aeedfe1e2205b810384ad18611cfc845590bc24f382abed26027fb9e"}
CREATE TRIGGER `audit_jsonb_insert` BEFORE INSERT ON `audit_events` FOR EACH ROW BEGIN IF (NEW.before_summary IS NOT NULL AND NOT ocserv_jsonb_value_valid(NEW.before_summary)) OR (NEW.after_summary IS NOT NULL AND NOT ocserv_jsonb_value_valid(NEW.after_summary)) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid audit JSON'; END IF; END;
-- ocservia:end-step
-- ocservia:step=118:trigger_certificate_jsonb_insert
-- ocservia:metadata={"kind":"trigger","object":"certificate_jsonb_insert","before":"","after":"ddd44daf8d76e6a3b474a15fb633921653270d10a0417fd1d72ceb19b3e5ec90"}
CREATE TRIGGER `certificate_jsonb_insert` BEFORE INSERT ON `certificates` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.dns_names) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid certificate JSON array'; END IF; END;
-- ocservia:end-step
-- ocservia:step=119:trigger_certificate_jsonb_update
-- ocservia:metadata={"kind":"trigger","object":"certificate_jsonb_update","before":"","after":"43c332e97b9e57a92c039952985c886f6864787ce6f7fb2d914df43d8013bb20"}
CREATE TRIGGER `certificate_jsonb_update` BEFORE UPDATE ON `certificates` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.dns_names) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid certificate JSON array'; END IF; END;
-- ocservia:end-step
-- ocservia:step=120:trigger_config_plan_jsonb_insert
-- ocservia:metadata={"kind":"trigger","object":"config_plan_jsonb_insert","before":"","after":"838904042e42da2cf634f08ef75458e6e04d63646356045701d221dc64f39493"}
CREATE TRIGGER `config_plan_jsonb_insert` BEFORE INSERT ON `config_plans` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.warnings) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid configuration warnings JSON array'; END IF; END;
-- ocservia:end-step
-- ocservia:step=121:trigger_config_plan_jsonb_update
-- ocservia:metadata={"kind":"trigger","object":"config_plan_jsonb_update","before":"","after":"5a23716d47378b81a8870bdfe82ba3ff7bfd661582f1bcbf2917bca815885c0d"}
CREATE TRIGGER `config_plan_jsonb_update` BEFORE UPDATE ON `config_plans` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.warnings) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid configuration warnings JSON array'; END IF; END;
-- ocservia:end-step
-- ocservia:step=122:trigger_desired_groups_logical_insert
-- ocservia:metadata={"kind":"trigger","object":"desired_groups_logical_insert","before":"","after":"e2f4d6b0cd4e74822762a39355eb637e109bac57bc7b21dfccce6557787b59f9"}
CREATE TRIGGER `desired_groups_logical_insert` BEFORE INSERT ON `desired_groups` FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.members) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
-- ocservia:end-step
-- ocservia:step=123:trigger_desired_groups_logical_update
-- ocservia:metadata={"kind":"trigger","object":"desired_groups_logical_update","before":"","after":"c7a7eb4ca6406808904ad46182ffc27da446c3c5a2c36d371722aa944daf1047"}
CREATE TRIGGER `desired_groups_logical_update` BEFORE UPDATE ON `desired_groups` FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.members) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
-- ocservia:end-step
-- ocservia:step=124:trigger_exact_agent_command_results_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_agent_command_results_insert","before":"","after":"388c8fe706c48ba378043e4e098d5d4abcae54f1efb69672c0f8b9f05491f95e"}
CREATE TRIGGER `exact_agent_command_results_insert` AFTER INSERT ON `agent_command_results` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='agent_command_results' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF NEW.`receipt_verification_status` = 'verified' AND NEW.`privd_attestation_key_id` IS NOT NULL AND NEW.`effect_record_id` IS NOT NULL AND NEW.`effect_sequence` IS NOT NULL THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`privd_attestation_key_id` AS BINARY))),16,'0')),CAST(NEW.`privd_attestation_key_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`effect_record_id` AS BINARY))),16,'0')),CAST(NEW.`effect_record_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`effect_sequence` AS BINARY))),16,'0')),CAST(NEW.`effect_sequence` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_agent_command_results` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_agent_command_results` (owner_id,key_value) VALUES(NEW.`event_id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=125:trigger_exact_agent_command_results_update
-- ocservia:metadata={"kind":"trigger","object":"exact_agent_command_results_update","before":"","after":"da04223681dca7dbef1c997f4ef5a9dd080d24f517e7a8faaac31ab3e38cb9d2"}
CREATE TRIGGER `exact_agent_command_results_update` AFTER UPDATE ON `agent_command_results` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='agent_command_results' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_agent_command_results` WHERE owner_id=NEW.`event_id`; IF NEW.`receipt_verification_status` = 'verified' AND NEW.`privd_attestation_key_id` IS NOT NULL AND NEW.`effect_record_id` IS NOT NULL AND NEW.`effect_sequence` IS NOT NULL THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`privd_attestation_key_id` AS BINARY))),16,'0')),CAST(NEW.`privd_attestation_key_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`effect_record_id` AS BINARY))),16,'0')),CAST(NEW.`effect_record_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`effect_sequence` AS BINARY))),16,'0')),CAST(NEW.`effect_sequence` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_agent_command_results` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_agent_command_results` (owner_id,key_value) VALUES(NEW.`event_id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=126:trigger_exact_identities_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_identities_insert","before":"","after":"12f478be8b01a733b0471ba352a8af08140a9528233b7c13bd4c944c8bfce398"}
CREATE TRIGGER `exact_identities_insert` AFTER INSERT ON `identities` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='identities' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`issuer` AS BINARY))),16,'0')),CAST(NEW.`issuer` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`subject` AS BINARY))),16,'0')),CAST(NEW.`subject` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_identities` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_identities` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=127:trigger_exact_identities_update
-- ocservia:metadata={"kind":"trigger","object":"exact_identities_update","before":"","after":"022af936bc17573dfb96c1a27ca6d59118d4b017b3fff73093d66e9b753bdf73"}
CREATE TRIGGER `exact_identities_update` AFTER UPDATE ON `identities` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='identities' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_identities` WHERE owner_id=NEW.`id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`issuer` AS BINARY))),16,'0')),CAST(NEW.`issuer` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`subject` AS BINARY))),16,'0')),CAST(NEW.`subject` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_identities` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_identities` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=128:trigger_exact_key_guards_immutable
-- ocservia:metadata={"kind":"trigger","object":"exact_key_guards_immutable","before":"","after":"fb775e6d1f824d62222eb32bea68c0f116a8c6af43480a8166608df5d33b1bc6"}
CREATE TRIGGER `exact_key_guards_immutable` BEFORE UPDATE ON `exact_key_guards` FOR EACH ROW SIGNAL SQLSTATE '42000' SET MYSQL_ERRNO=1142,MESSAGE_TEXT='exact-key guards are immutable';
-- ocservia:end-step
-- ocservia:step=129:trigger_exact_nodes_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_nodes_insert","before":"","after":"4c3657b81b3f9065ecc1c2c7d1766bc6984f7d7685ac673fc9527a0766230f8b"}
CREATE TRIGGER `exact_nodes_insert` AFTER INSERT ON `nodes` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='nodes' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`workspace_id` AS BINARY))),16,'0')),CAST(NEW.`workspace_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`name` AS BINARY))),16,'0')),CAST(NEW.`name` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_nodes` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_nodes` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=130:trigger_exact_nodes_update
-- ocservia:metadata={"kind":"trigger","object":"exact_nodes_update","before":"","after":"9bfc38b7b99c4ec41a438d7921d06bdd223016fa8ac01b6ff1c02de6b70484d7"}
CREATE TRIGGER `exact_nodes_update` AFTER UPDATE ON `nodes` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='nodes' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_nodes` WHERE owner_id=NEW.`id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`workspace_id` AS BINARY))),16,'0')),CAST(NEW.`workspace_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`name` AS BINARY))),16,'0')),CAST(NEW.`name` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_nodes` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_nodes` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=131:trigger_exact_operations_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_operations_insert","before":"","after":"9d1d9c2151de68859034c62c99801e011ae255841412b4140dbc38a46ba9b056"}
CREATE TRIGGER `exact_operations_insert` AFTER INSERT ON `operations` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='operations' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF NEW.`idempotency_key` IS NOT NULL THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`workspace_id` AS BINARY))),16,'0')),CAST(NEW.`workspace_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`idempotency_key` AS BINARY))),16,'0')),CAST(NEW.`idempotency_key` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_operations` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_operations` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=132:trigger_exact_operations_update
-- ocservia:metadata={"kind":"trigger","object":"exact_operations_update","before":"","after":"a4f5aead4a5cd204e36ecc1b2ba1f5f33c25693e201cb8eb185eea8ab2efac9b"}
CREATE TRIGGER `exact_operations_update` AFTER UPDATE ON `operations` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='operations' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_operations` WHERE owner_id=NEW.`id`; IF NEW.`idempotency_key` IS NOT NULL THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`workspace_id` AS BINARY))),16,'0')),CAST(NEW.`workspace_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`idempotency_key` AS BINARY))),16,'0')),CAST(NEW.`idempotency_key` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_operations` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_operations` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=133:trigger_exact_telemetry_rollups_1h_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_telemetry_rollups_1h_insert","before":"","after":"b6636eef80b102a5ada95c4aa0b1b9bf1e1da4cc1f9a0928cef0f3f4844760d7"}
CREATE TRIGGER `exact_telemetry_rollups_1h_insert` AFTER INSERT ON `telemetry_rollups_1h` FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='telemetry_rollups_1h' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`metric` AS BINARY))),16,'0')),CAST(NEW.`metric` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`bucket_at` AS BINARY))),16,'0')),CAST(NEW.`bucket_at` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM exact_telemetry_rollups_1h WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO exact_telemetry_rollups_1h(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END;
-- ocservia:end-step
-- ocservia:step=134:trigger_exact_telemetry_rollups_1h_update
-- ocservia:metadata={"kind":"trigger","object":"exact_telemetry_rollups_1h_update","before":"","after":"f9f3b0d96bdea6a2ffee17c44ee07c9065320d9c8ae2d337ff50c9921754a98a"}
CREATE TRIGGER `exact_telemetry_rollups_1h_update` AFTER UPDATE ON `telemetry_rollups_1h` FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; IF NEW.node_id=OLD.node_id AND BINARY NEW.metric=BINARY OLD.metric AND NEW.bucket_at=OLD.bucket_at THEN LEAVE main; END IF; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='telemetry_rollups_1h' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM exact_telemetry_rollups_1h WHERE owner_id=NEW.exact_row_id; IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`metric` AS BINARY))),16,'0')),CAST(NEW.`metric` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`bucket_at` AS BINARY))),16,'0')),CAST(NEW.`bucket_at` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM exact_telemetry_rollups_1h WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO exact_telemetry_rollups_1h(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END;
-- ocservia:end-step
-- ocservia:step=135:trigger_exact_telemetry_rollups_5m_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_telemetry_rollups_5m_insert","before":"","after":"0ee0d41a1b329d4cc254ccaa80ddfcdfe3582bbb8c340c031021dd2a2cde193e"}
CREATE TRIGGER `exact_telemetry_rollups_5m_insert` AFTER INSERT ON `telemetry_rollups_5m` FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='telemetry_rollups_5m' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`metric` AS BINARY))),16,'0')),CAST(NEW.`metric` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`bucket_at` AS BINARY))),16,'0')),CAST(NEW.`bucket_at` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM exact_telemetry_rollups_5m WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO exact_telemetry_rollups_5m(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END;
-- ocservia:end-step
-- ocservia:step=136:trigger_exact_telemetry_rollups_5m_update
-- ocservia:metadata={"kind":"trigger","object":"exact_telemetry_rollups_5m_update","before":"","after":"548aed7a5e86642d996b2c9e1b8adcf16a7a514c05d00621a3f2965a0f197184"}
CREATE TRIGGER `exact_telemetry_rollups_5m_update` AFTER UPDATE ON `telemetry_rollups_5m` FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; IF NEW.node_id=OLD.node_id AND BINARY NEW.metric=BINARY OLD.metric AND NEW.bucket_at=OLD.bucket_at THEN LEAVE main; END IF; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='telemetry_rollups_5m' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM exact_telemetry_rollups_5m WHERE owner_id=NEW.exact_row_id; IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`metric` AS BINARY))),16,'0')),CAST(NEW.`metric` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`bucket_at` AS BINARY))),16,'0')),CAST(NEW.`bucket_at` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM exact_telemetry_rollups_5m WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO exact_telemetry_rollups_5m(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END;
-- ocservia:end-step
-- ocservia:step=137:trigger_exact_upstream_sync_records_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_upstream_sync_records_insert","before":"","after":"312befad3876c4d23325bd3bc2aee71ecd029df4c592cd186dc52bf6937cec73"}
CREATE TRIGGER `exact_upstream_sync_records_insert` AFTER INSERT ON `upstream_sync_records` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='upstream_sync_records' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`repository` AS BINARY))),16,'0')),CAST(NEW.`repository` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`old_commit` AS BINARY))),16,'0')),CAST(NEW.`old_commit` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`new_commit` AS BINARY))),16,'0')),CAST(NEW.`new_commit` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_upstream_sync_records` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_upstream_sync_records` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=138:trigger_exact_upstream_sync_records_update
-- ocservia:metadata={"kind":"trigger","object":"exact_upstream_sync_records_update","before":"","after":"8cd480eb0a29c3e704487826503cd0be9a064a05478ff7e99e176b4e4004440a"}
CREATE TRIGGER `exact_upstream_sync_records_update` AFTER UPDATE ON `upstream_sync_records` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='upstream_sync_records' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_upstream_sync_records` WHERE owner_id=NEW.`id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`repository` AS BINARY))),16,'0')),CAST(NEW.`repository` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`old_commit` AS BINARY))),16,'0')),CAST(NEW.`old_commit` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`new_commit` AS BINARY))),16,'0')),CAST(NEW.`new_commit` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_upstream_sync_records` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_upstream_sync_records` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=139:trigger_exact_user_policy_enforcements_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_user_policy_enforcements_insert","before":"","after":"dc6c78e0a3fbc204e6d790ce35c4aba540fa5fbc11588c3bba9ba5b1ec138065"}
CREATE TRIGGER `exact_user_policy_enforcements_insert` AFTER INSERT ON `user_policy_enforcements` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='user_policy_enforcements' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`username` AS BINARY))),16,'0')),CAST(NEW.`username` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`policy_version` AS BINARY))),16,'0')),CAST(NEW.`policy_version` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`cause` AS BINARY))),16,'0')),CAST(NEW.`cause` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`period_start` AS BINARY))),16,'0')),CAST(NEW.`period_start` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_user_policy_enforcements` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_user_policy_enforcements` (owner_id,key_value) VALUES(NEW.`exact_row_id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=140:trigger_exact_user_policy_enforcements_update
-- ocservia:metadata={"kind":"trigger","object":"exact_user_policy_enforcements_update","before":"","after":"224185a504dee08a0ea8f1e45f07b23d498e65d84c3c52393da4af862f1c1c34"}
CREATE TRIGGER `exact_user_policy_enforcements_update` AFTER UPDATE ON `user_policy_enforcements` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='user_policy_enforcements' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_user_policy_enforcements` WHERE owner_id=NEW.`exact_row_id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`node_id` AS BINARY))),16,'0')),CAST(NEW.`node_id` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`username` AS BINARY))),16,'0')),CAST(NEW.`username` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`policy_version` AS BINARY))),16,'0')),CAST(NEW.`policy_version` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`cause` AS BINARY))),16,'0')),CAST(NEW.`cause` AS BINARY),UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`period_start` AS BINARY))),16,'0')),CAST(NEW.`period_start` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_user_policy_enforcements` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_user_policy_enforcements` (owner_id,key_value) VALUES(NEW.`exact_row_id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=141:trigger_exact_workspaces_insert
-- ocservia:metadata={"kind":"trigger","object":"exact_workspaces_insert","before":"","after":"0c620299ee8ad470c1850f237c1e98f2210a1031e4e13dda1910a4d621339f4c"}
CREATE TRIGGER `exact_workspaces_insert` AFTER INSERT ON `workspaces` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='workspaces' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`slug` AS BINARY))),16,'0')),CAST(NEW.`slug` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_workspaces` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_workspaces` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=142:trigger_exact_workspaces_update
-- ocservia:metadata={"kind":"trigger","object":"exact_workspaces_update","before":"","after":"fc2489b86602f5b7cbe4e722c424e142fa4896bd74aa7457a71e886b3c87c009"}
CREATE TRIGGER `exact_workspaces_update` AFTER UPDATE ON `workspaces` FOR EACH ROW BEGIN DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner VARBINARY(16); DECLARE encoded LONGBLOB; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='workspaces' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; DELETE FROM `exact_workspaces` WHERE owner_id=NEW.`id`; IF TRUE THEN SET encoded=CONCAT(UNHEX(LPAD(HEX(OCTET_LENGTH(CAST(NEW.`slug` AS BINARY))),16,'0')),CAST(NEW.`slug` AS BINARY)); SELECT owner_id INTO duplicate_owner FROM `exact_workspaces` WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO `exact_workspaces` (owner_id,key_value) VALUES(NEW.`id`,encoded); END IF; END;
-- ocservia:end-step
-- ocservia:step=143:trigger_nodes_jsonb_insert
-- ocservia:metadata={"kind":"trigger","object":"nodes_jsonb_insert","before":"","after":"2db2db59926175477e0546e940f138d64950d5b5f287500bafb4553d6535e013"}
CREATE TRIGGER `nodes_jsonb_insert` BEFORE INSERT ON `nodes` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_value_valid(NEW.`labels`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END;
-- ocservia:end-step
-- ocservia:step=144:trigger_nodes_jsonb_update
-- ocservia:metadata={"kind":"trigger","object":"nodes_jsonb_update","before":"","after":"926fc2a745b275daf9a431ae108273ed0f7a3054b8899cf7267d62e2b0d5797d"}
CREATE TRIGGER `nodes_jsonb_update` BEFORE UPDATE ON `nodes` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_value_valid(NEW.`labels`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END;
-- ocservia:end-step
-- ocservia:step=145:trigger_node_snapshot_jsonb_insert
-- ocservia:metadata={"kind":"trigger","object":"node_snapshot_jsonb_insert","before":"","after":"a42389a9e03edf80a43df7fc50cd103de12db03033477ba641385f857caca96f"}
CREATE TRIGGER `node_snapshot_jsonb_insert` BEFORE INSERT ON `node_observed_snapshots` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.ocserv) OR NOT ocserv_jsonb_object_valid(NEW.system) OR NOT ocserv_jsonb_object_valid(NEW.path) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid snapshot JSON object'; END IF; END;
-- ocservia:end-step
-- ocservia:step=146:trigger_node_snapshot_jsonb_update
-- ocservia:metadata={"kind":"trigger","object":"node_snapshot_jsonb_update","before":"","after":"1330827431f2ebd5529753b43ce949f65ae667d98a6731e1865c6587b26f3d8d"}
CREATE TRIGGER `node_snapshot_jsonb_update` BEFORE UPDATE ON `node_observed_snapshots` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.ocserv) OR NOT ocserv_jsonb_object_valid(NEW.system) OR NOT ocserv_jsonb_object_valid(NEW.path) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid snapshot JSON object'; END IF; END;
-- ocservia:end-step
-- ocservia:step=147:trigger_observed_groups_logical_insert
-- ocservia:metadata={"kind":"trigger","object":"observed_groups_logical_insert","before":"","after":"d90a58354e79a9ce1fd1ea494a7fa51fc21e77bf3a2b7a40f81f435917b9170c"}
CREATE TRIGGER `observed_groups_logical_insert` BEFORE INSERT ON `observed_groups` FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.`members`) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
-- ocservia:end-step
-- ocservia:step=148:trigger_observed_groups_logical_update
-- ocservia:metadata={"kind":"trigger","object":"observed_groups_logical_update","before":"","after":"870d5fb66f16eca695a60d4876a8312718a573795b0f15518d89e200702fb124"}
CREATE TRIGGER `observed_groups_logical_update` BEFORE UPDATE ON `observed_groups` FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.`members`) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
-- ocservia:end-step
-- ocservia:step=149:trigger_rollout_jsonb_insert
-- ocservia:metadata={"kind":"trigger","object":"rollout_jsonb_insert","before":"","after":"beeb2867a49fcf468ee4a3736e8abb8b4646d66df8d9bdf2acd0c808379bef4a"}
CREATE TRIGGER `rollout_jsonb_insert` BEFORE INSERT ON `agent_rollouts` FOR EACH ROW BEGIN IF NOT ocserv_rollout_exclusions_valid(NEW.exclusions) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid rollout exclusions JSON array'; END IF; END;
-- ocservia:end-step
-- ocservia:step=150:trigger_rollout_jsonb_update
-- ocservia:metadata={"kind":"trigger","object":"rollout_jsonb_update","before":"","after":"6380761b964605df6d5e073b0db4ffdbd5784ac0057c047e26e92f9842df6d93"}
CREATE TRIGGER `rollout_jsonb_update` BEFORE UPDATE ON `agent_rollouts` FOR EACH ROW BEGIN IF NOT ocserv_rollout_exclusions_valid(NEW.exclusions) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid rollout exclusions JSON array'; END IF; END;
-- ocservia:end-step
-- ocservia:step=151:trigger_telemetry_legacy_insert_guard
-- ocservia:metadata={"kind":"trigger","object":"telemetry_legacy_insert_guard","before":"","after":"1ac292bbd2da5520287071b9c24011d5ab055d3fb540671033ac23ccf84223f1"}
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
-- ocservia:end-step
-- ocservia:step=152:trigger_telemetry_legacy_update_guard
-- ocservia:metadata={"kind":"trigger","object":"telemetry_legacy_update_guard","before":"","after":"6774adaa77d2955a5b139cf07bee40a8d09a2aadc5a0bf725fd146bf21d83ccf"}
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
-- ocservia:end-step
-- ocservia:step=153:trigger_telemetry_security_events_logical_insert
-- ocservia:metadata={"kind":"trigger","object":"telemetry_security_events_logical_insert","before":"","after":"63e71d007899b4f224711231d036720f3852c2065e5dbcf4b1e7301e468861ef"}
CREATE TRIGGER `telemetry_security_events_logical_insert` BEFORE INSERT ON `telemetry_security_events` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.`detail`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
-- ocservia:end-step
-- ocservia:step=154:trigger_telemetry_security_events_logical_update
-- ocservia:metadata={"kind":"trigger","object":"telemetry_security_events_logical_update","before":"","after":"be290c56de4fd1a1d824f755101c7f6b1b1398b7e7e277f17a3c867a37279b14"}
CREATE TRIGGER `telemetry_security_events_logical_update` BEFORE UPDATE ON `telemetry_security_events` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.`detail`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END;
-- ocservia:end-step
-- ocservia:step=155:trigger_upstream_sync_records_jsonb_insert
-- ocservia:metadata={"kind":"trigger","object":"upstream_sync_records_jsonb_insert","before":"","after":"d651f264d424c3f7039f849cf80f1d20efcc53770132feca20ef8c2da8fab8bb"}
CREATE TRIGGER `upstream_sync_records_jsonb_insert` BEFORE INSERT ON `upstream_sync_records` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.`classification`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END;
-- ocservia:end-step
-- ocservia:step=156:trigger_upstream_sync_records_jsonb_update
-- ocservia:metadata={"kind":"trigger","object":"upstream_sync_records_jsonb_update","before":"","after":"9250ce97f1757d710b1c3f10aeab011a5b41f2a61f9ae971d6e766cfe779aa61"}
CREATE TRIGGER `upstream_sync_records_jsonb_update` BEFORE UPDATE ON `upstream_sync_records` FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.`classification`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END;
-- ocservia:end-step
