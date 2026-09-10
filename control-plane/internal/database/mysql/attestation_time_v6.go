package mysql

import (
	"fmt"
	"strings"
)

// AttestationTimeSteps converts the privd attestation credential and key
// times to checked PostgreSQL-epoch microseconds. The enrollment, registration
// and revocation writers, the telemetry ingestion key read, and the constraint
// definitions move together with this version. Published revisions stay
// byte-for-byte unchanged.
func AttestationTimeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	clock := "(TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',CURRENT_TIMESTAMP(6)))"
	// Unlimited key validity is NULL on the original domain, never an infinity,
	// so every non-NULL source value converts as a finite instant.
	credentials := timeColumnSteps("privd_attestation_enrollment_credentials",
		[]TimeColumn{{Name: "expires_at"}, {Name: "consumed_at", Nullable: true}, {Name: "created_at", DefaultSQL: clock}},
		"HEX(source.id)",
		[]string{
			drop + "privd_attestation_enrollment_credentials_privd_atte_96cebf9d41c4",
			drop + "privd_attestation_enrollment_credentials_privd_atte_4f7a46e8df72",
			"DROP INDEX privd_attestation_credentials_node_active_idx",
		},
		[]string{
			"ADD CONSTRAINT privd_attestation_enrollment_credentials_privd_atte_96cebf9d41c4 CHECK (expires_at>created_at)",
			"ADD CONSTRAINT privd_attestation_enrollment_credentials_privd_atte_4f7a46e8df72 CHECK (consumed_at IS NULL OR consumed_at>=created_at)",
			"ADD KEY privd_attestation_credentials_node_active_idx(node_id,expires_at)",
		})
	keys := timeColumnSteps("node_privd_attestation_keys",
		[]TimeColumn{{Name: "created_at"}, {Name: "approved_at"}, {Name: "activated_at"}, {Name: "valid_until", Nullable: true}, {Name: "revoked_at", Nullable: true}},
		"CONCAT(HEX(source.node_id),':',source.key_id)",
		[]string{
			drop + "node_privd_attestation_keys_node_privd_attestation_keys_check",
			drop + "node_privd_attestation_keys_node_privd_attestation_keys_check1",
			"DROP INDEX node_privd_attestation_keys_active_idx",
		},
		[]string{
			"ADD CONSTRAINT node_privd_attestation_keys_node_privd_attestation_keys_check CHECK (valid_until IS NULL OR valid_until>=activated_at)",
			"ADD CONSTRAINT node_privd_attestation_keys_node_privd_attestation_keys_check1 CHECK ((state='revoked')=(revoked_at IS NOT NULL))",
			"ADD KEY node_privd_attestation_keys_active_idx(node_id,state,activated_at)",
		})
	return append(guardNamedTimeSteps("migrate_v6_attest_credential", "privd_attestation_enrollment_credentials", credentials),
		guardNamedTimeSteps("migrate_v6_attest_key", "node_privd_attestation_keys", keys)...)
}

// guardNamedTimeSteps is GuardTimeSteps with an explicit trigger-name prefix:
// the enrollment credential table name is too long for the generated
// drop_<trigger> identifier limit shared by MySQL/MariaDB and the revision
// plan validator.
func guardNamedTimeSteps(name, table string, changes []LongKeyStep) []LongKeyStep {
	var steps []LongKeyStep
	for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
		trigger := name + "_" + strings.ToLower(event)
		steps = append(steps, LongKeyStep{Name: trigger, Kind: "trigger", Object: trigger, SQL: fmt.Sprintf("CREATE TRIGGER `%s` BEFORE %s ON `%s` FOR EACH ROW BEGIN DECLARE holder BIGINT; SET holder=IS_USED_LOCK(CONCAT('ocservia:',LEFT(SHA2(DATABASE(),256),48))); IF holder IS NULL OR holder<>CONNECTION_ID() THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='time migration requires exclusive writer'; END IF; END", trigger, event, table)})
	}
	steps = append(steps, changes...)
	for _, event := range []string{"insert", "update", "delete"} {
		trigger := name + "_" + event
		steps = append(steps, LongKeyStep{Name: "drop_" + trigger, Kind: "trigger", Object: trigger, SQL: "DROP TRIGGER `" + trigger + "`"})
	}
	return steps
}
