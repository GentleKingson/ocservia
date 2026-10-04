package migrations

import "context"

// Classification is read-only and runs under the same lock as initialization.
// public, system schemas and the default plpgsql extension may preexist.
// Namespace dependencies also cover collations, operators, conversions and
// text-search objects, without assuming that every user object is a relation.
// Database-global objects need separate catalog checks. These catalogs remain
// readable by a normal database owner, including when no superuser is used.
func databaseState(ctx context.Context, db queryer) (empty bool, err error) {
	err = db.QueryRow(ctx, `SELECT NOT (EXISTS(SELECT 1 FROM pg_namespace WHERE nspname <> 'public' AND nspname <> 'information_schema' AND nspname !~ '^pg_')
 OR EXISTS(SELECT 1 FROM pg_depend d JOIN pg_namespace n ON d.refclassid='pg_namespace'::regclass AND d.refobjid=n.oid WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_')
 OR EXISTS(SELECT 1 FROM pg_extension WHERE extname <> 'plpgsql')
 OR EXISTS(SELECT 1 FROM pg_event_trigger)
 OR EXISTS(SELECT 1 FROM pg_foreign_data_wrapper)
 OR EXISTS(SELECT 1 FROM pg_foreign_server)
 OR EXISTS(SELECT 1 FROM pg_publication)
 OR EXISTS(SELECT oid FROM pg_subscription WHERE subdbid=(SELECT oid FROM pg_database WHERE datname=current_database()))
 OR EXISTS(SELECT 1 FROM pg_largeobject_metadata)
 OR EXISTS(SELECT 1 FROM pg_default_acl)
 OR EXISTS(SELECT 1 FROM pg_depend WHERE classid IN ('pg_cast'::regclass,'pg_transform'::regclass))
 OR EXISTS(SELECT 1 FROM pg_language WHERE lanname NOT IN ('internal','c','sql','plpgsql')))
`).Scan(&empty)
	return
}
