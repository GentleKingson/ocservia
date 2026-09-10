package mysql

import (
	"fmt"
	"strings"
)

// CertificateTypeSteps appends the remaining certificate timestamp and JSONB
// conversions. Published v1-v6 SQL and receipts are never rewritten.
func CertificateTypeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	validator := strings.Replace(jsonbObjectValidator, "ocserv_jsonb_object_valid", "ocserv_jsonb_array_valid", 1)
	validator = strings.Replace(validator, "_binary'OBJECT'", "_binary'ARRAY'", 1)
	steps := []LongKeyStep{{Name: "jsonb_array_validator", Kind: "function", Object: "ocserv_jsonb_array_valid", SQL: validator}}
	changes := timeColumnSteps("certificates", []TimeColumn{
		{Name: "not_before", Nullable: true}, {Name: "revoked_at", Nullable: true},
		{Name: "created_at"}, {Name: "updated_at"}, {Name: "csr_receipt_verified_at", Nullable: true},
	}, "HEX(source.id)", []string{drop + "certificates_certificates_csr_receipt_check"}, []string{
		`ADD CONSTRAINT certificates_certificates_csr_receipt_check CHECK (((state IN ('csr_ready','signing','signer_unavailable','issued','expiring','expired','revoking','revocation_unknown','revoked')) AND csr_receipt_verified_at IS NOT NULL AND OCTET_LENGTH(csr_receipt_sha256)=32 AND csr_privd_attestation_key_id IS NOT NULL AND OCTET_LENGTH(csr_effect_record_id) BETWEEN 16 AND 32 AND OCTET_LENGTH(csr_der_sha256)=32 AND OCTET_LENGTH(csr_requested_subject_sha256)=32) OR csr_receipt_legacy OR state IN ('csr_pending','failed','unknown'))`,
	})
	check := `SELECT IF(NOT EXISTS(SELECT 1 FROM certificates WHERE NOT (logical_dns_names <=> CAST(dns_names AS BINARY)) OR NOT ocserv_jsonb_array_valid(logical_dns_names)),'valid','invalid')`
	changes = append(changes,
		LongKeyStep{Name: "certificate_dns_names_blob", Kind: "table", Object: "certificates", SQL: `ALTER TABLE certificates ADD COLUMN logical_dns_names LONGBLOB NULL`},
		LongKeyStep{Name: "certificate_dns_names_copy", Kind: "data", Object: "certificates", Repairable: true, SQL: `UPDATE certificates SET logical_dns_names=CAST(dns_names AS BINARY)`, VerifySQL: check},
		LongKeyStep{Name: "certificate_dns_names_switch", Kind: "table", Object: "certificates", CheckBeforeSQL: check, SQL: `ALTER TABLE certificates ` + drop + `certificates_certificates_dns_names_check,DROP COLUMN dns_names,CHANGE COLUMN logical_dns_names dns_names LONGBLOB NOT NULL DEFAULT ('[]')`},
	)
	for _, event := range []string{"INSERT", "UPDATE"} {
		name := "certificate_jsonb_" + strings.ToLower(event)
		changes = append(changes, LongKeyStep{Name: name, Kind: "trigger", Object: name, SQL: fmt.Sprintf("CREATE TRIGGER `%s` BEFORE %s ON certificates FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.dns_names) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid certificate JSON array'; END IF; END", name, event)})
	}
	return append(steps, GuardTimeSteps(changes, "certificates")...)
}
