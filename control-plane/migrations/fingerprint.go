package migrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// staticFingerprintSQL excludes owner-managed calendar partition leaves and
// runtime grants, which vary after initialization. It retains static columns,
// constraints, indexes, routines, triggers, sequences and namespace identity.
// Snapshot authoring executes this same read-only query on the actual replay.
const staticFingerprintSQL = `WITH objects AS (
 SELECT c.oid,c.relname,c.relkind,c.reloptions
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND NOT c.relispartition AND c.relkind IN ('r','p','v','m','f','S')
), columns AS (
 SELECT c.relname,a.attname,a.attnum,format_type(a.atttypid,a.atttypmod) AS type,
 a.attnotnull,a.attidentity,a.attgenerated,co.collname,pg_get_expr(d.adbin,d.adrelid) AS default_value
 FROM objects c JOIN pg_attribute a ON a.attrelid=c.oid
 LEFT JOIN pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum
 LEFT JOIN pg_collation co ON co.oid=a.attcollation
 WHERE a.attnum>0 AND NOT a.attisdropped
), constraints AS (
 SELECT c.relname,x.conname,x.contype,x.convalidated,x.condeferrable,x.condeferred,
 pg_get_constraintdef(x.oid,true) AS definition
 FROM objects c JOIN pg_constraint x ON x.conrelid=c.oid
), indexes AS (
 SELECT c.relname,i.relname AS name,x.indisvalid,x.indisready,pg_get_indexdef(i.oid) AS definition
 FROM objects c JOIN pg_index x ON x.indrelid=c.oid JOIN pg_class i ON i.oid=x.indexrelid
), routines AS (
 SELECT p.proname,pg_get_function_identity_arguments(p.oid) AS args,
 pg_get_functiondef(p.oid) AS definition
 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'
), triggers AS (
 SELECT c.relname,t.tgname,t.tgenabled,pg_get_triggerdef(t.oid,true) AS definition
 FROM objects c JOIN pg_trigger t ON t.tgrelid=c.oid WHERE NOT t.tgisinternal
), sequences AS (
 SELECT c.relname,s.seqtypid::regtype::text AS type,s.seqstart,s.seqincrement,s.seqmax,s.seqmin,s.seqcache,s.seqcycle
 FROM objects c JOIN pg_sequence s ON s.seqrelid=c.oid
), definitions AS (
 SELECT c.relname,c.relkind,c.reloptions,
 CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid,true) END AS view,
 CASE WHEN c.relkind='p' THEN pg_get_partkeydef(c.oid) END AS partition_key
 FROM objects c
)
SELECT jsonb_build_object(
 'namespaces',(SELECT coalesce(jsonb_agg(nspname ORDER BY nspname),'[]') FROM pg_namespace WHERE nspname<>'information_schema' AND nspname!~'^pg_'),
 'objects',(SELECT coalesce(jsonb_agg(to_jsonb(d) ORDER BY relname),'[]') FROM definitions d),
 'columns',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY relname,attnum),'[]') FROM columns c),
 'constraints',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY relname,conname),'[]') FROM constraints c),
 'indexes',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY relname,name),'[]') FROM indexes i),
 'routines',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY proname,args),'[]') FROM routines r),
 'triggers',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY relname,tgname),'[]') FROM triggers t),
 'sequences',(SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY relname),'[]') FROM sequences s)
)::text`

func validateStaticSchema(ctx context.Context, db queryer, expected string) error {
	var catalog string
	if err := db.QueryRow(ctx, staticFingerprintSQL).Scan(&catalog); err != nil {
		return fmt.Errorf("inspect PostgreSQL checkpoint schema: %w", err)
	}
	sum := sha256.Sum256([]byte(catalog))
	if hex.EncodeToString(sum[:]) != expected {
		return errors.New("PostgreSQL schema differs from checkpoint artifact")
	}
	return nil
}

// A transition verifies the new business schema and journal before the old
// metadata tables are removed. No other object is omitted from admission.
func validateRevisionSchema(ctx context.Context, db queryer, expected string, transition bool) error {
	if !transition {
		return validateStaticSchema(ctx, db, expected)
	}
	query := strings.Replace(staticFingerprintSQL, "AND NOT c.relispartition", "AND c.relname NOT IN ('schema_migrations','schema_snapshot_origin') AND NOT c.relispartition", 1)
	var catalog string
	if err := db.QueryRow(ctx, query).Scan(&catalog); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(catalog))
	if hex.EncodeToString(sum[:]) != expected {
		return errors.New("PostgreSQL schema differs from new checkpoint artifact")
	}
	return nil
}
