#!/usr/bin/env bash
# Reconstruct the bounded window from the released checkpoint, then export
# final objects. History is read from Git; no archive artifact is maintained.
# check never overwrites checked-in artifacts.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-check}"
case "$mode" in generate|check) ;; *) echo 'usage: database-postgres-snapshot.sh [generate|check]' >&2; exit 2;; esac
image='postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af'
name="ocservia-pg-snapshot-$$"
tmp="$(mktemp -d)"
cleanup() { docker rm -fv "$name" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT
docker run -d --name "$name" -e POSTGRES_USER=ocservia_owner -e POSTGRES_PASSWORD=snapshot-test-only -e POSTGRES_DB=snapshot "$image" >/dev/null
ready=false
for ((i=0;i<60;i++)); do
 if docker exec -e PGPASSWORD=snapshot-test-only "$name" psql -X -h127.0.0.1 -U ocservia_owner -d snapshot -Atc 'SELECT 1' >/dev/null 2>&1; then ready=true; break; fi
 sleep 1
done
[[ "$ready" == true ]] || { echo 'snapshot PostgreSQL not ready' >&2; exit 1; }
psql() { docker exec -i "$name" psql -X -v ON_ERROR_STOP=1 -U ocservia_owner -d snapshot "$@"; }
checkpoint_tag=v1.2.0
checkpoint_sha=169102557cd610847c9f6ac2083336cdcf82c483
checkpoint_receipt=d837335f22c70858f4e2a5277332e478ecd6032e7b55f57d6512484bfab6f172
if ! git -C "$ROOT" rev-parse --verify "${checkpoint_tag}^{commit}" >/dev/null 2>&1; then
 git -C "$ROOT" fetch origin "refs/tags/${checkpoint_tag}:refs/tags/${checkpoint_tag}"
fi
[[ "$(git -C "$ROOT" rev-parse "${checkpoint_tag}^{commit}")" == "$checkpoint_sha" ]] || { echo 'designated checkpoint tag moved' >&2; exit 1; }
git -C "$ROOT" show "$checkpoint_sha:control-plane/migrations/schema.sql" > "$tmp/checkpoint.sql"
[[ "$(sha256sum "$tmp/checkpoint.sql" | cut -d' ' -f1)" == "$checkpoint_receipt" ]] || { echo 'published checkpoint receipt changed' >&2; exit 1; }
psql -c 'CREATE ROLE ocservia_app' >/dev/null
psql --single-transaction < "$tmp/checkpoint.sql" >/dev/null
psql -c "INSERT INTO schema_revisions(epoch,revision,checksum,state,step,verified_at) VALUES(1,0,decode('$checkpoint_receipt','hex'),'verified',1,now())" >/dev/null
# Authoring replays the bounded SQL independently. The real executor's
# admission, verified-before-cleanup and rollback are exercised by the required
# PostgreSQL lifecycle tests; this export never stamps historical execution.
python3 - "$ROOT/control-plane/migrations" "$tmp/replay.sql" <<'PY_REPLAY'
import hashlib,json,pathlib,re,sys
root=pathlib.Path(sys.argv[1]);upgrade=(root/'upgrade.sql').read_text()
base=json.loads(re.search(r'^-- ocservia:baseline=(.+)$',upgrade,re.M).group(1))
transition=upgrade[upgrade.index('-- ocservia:transition='):upgrade.index('-- ocservia:end-transition')]
first,cleanup=transition.split('-- ocservia:step=002:legacy_cleanup\n',1)
# All SQL comes from the two active artifacts; journal publication precedes
# the encoded cleanup statement, in the same PostgreSQL transaction.
receipt="INSERT INTO schema_revisions(epoch,revision,checksum,state,step,verified_at) VALUES(2,0,decode('%s','hex'),'verified',%d,now());\n"%(base['checksum'],base['steps'])
pathlib.Path(sys.argv[2]).write_text('BEGIN;\n'+first+receipt+cleanup+'COMMIT;\n')
PY_REPLAY
psql < "$tmp/replay.sql" >/dev/null
# There are no business rows in this isolated replay. Calendar partitions are
# runtime provisioning, never part of a reproducible static snapshot.
psql -Atc "SELECT format('DROP TABLE %I.%I;', n.nspname,c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname ~ '^telemetry_samples_[0-9]{6}$' ORDER BY c.relname" | psql >/dev/null
docker exec "$name" pg_dump -U ocservia_owner -d snapshot --schema-only --no-owner --schema=public > "$tmp/schema.raw"
docker exec "$name" pg_dump -U ocservia_owner -d snapshot --data-only --no-owner --column-inserts --table=roles --table=upstream_sync_records --table=controller_schema_compatibility > "$tmp/seeds.raw"
# Export deterministic fields from the actual replay; updated_at intentionally
# uses its schema default at initialization time, just as migration 24 does.
psql -Atc "SELECT format('INSERT INTO public.scheduler_leadership(id,instance_id,incarnation,epoch,lease_until) VALUES(%s,%L,%s,%s,%L);',id,instance_id,incarnation,epoch,lease_until) FROM scheduler_leadership ORDER BY id" > "$tmp/scheduler.sql"
psql -Atc "SELECT coalesce(json_agg(json_build_object('table',c.relname,'column',a.attname,'name',x.conname) ORDER BY c.relname,a.attnum),'[]') FROM pg_constraint x JOIN pg_class c ON c.oid=x.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=x.conkey[1] WHERE x.contype='n' AND n.nspname='public'" > "$tmp/not-null.json"
python3 - "$ROOT/control-plane/migrations/fingerprint.go" "$tmp/catalog.sql" <<'PY_CATALOG'
import pathlib,re,sys
source=pathlib.Path(sys.argv[1]).read_text()
query=re.search(r'const staticFingerprintSQL = `([^`]+)`',source).group(1)
pathlib.Path(sys.argv[2]).write_text(query+';\n')
PY_CATALOG
psql -At -f - < "$tmp/catalog.sql" > "$tmp/catalog.json"
python3 - "$ROOT/control-plane/migrations" "$tmp" <<'PY'
import hashlib,json,pathlib,re,sys
root,tmp=map(pathlib.Path,sys.argv[1:])
def normalize(s):
 lines=[]
 for line in s.splitlines():
  if line.startswith('--') or line.startswith('\\') or line.startswith('CREATE SCHEMA public;') or line.startswith('COMMENT ON SCHEMA public '): continue
  if line.startswith('SET '): line=line.replace('SET ','SET LOCAL ',1)
  if line.startswith("SELECT pg_catalog.set_config("): line=line.replace(', false);', ', true);')
  lines.append(line.rstrip())
 return re.sub(r'\n{3,}','\n\n','\n'.join(lines)).strip()+'\n'
# PostgreSQL 18 pg_dump may omit implicit NOT NULL names even when LIKE has
# copied that name to another table. Make their actual names explicit so name
# allocation order on replay cannot append a different numeric suffix.
notnull={(r['table'],r['column']):r['name'] for r in json.loads((tmp/'not-null.json').read_text())}
lines=[];table=None
for line in (tmp/'schema.raw').read_text().splitlines():
 m=re.match(r'CREATE TABLE public\.(\w+) \(',line)
 if m: table=m[1]
 if table and 'NOT NULL' in line and 'CONSTRAINT ' not in line:
  m=re.match(r'    (\w+) ',line)
  if m and (table,m[1]) in notnull:
   name=notnull[table,m[1]]
   line=line.replace('NOT NULL','CONSTRAINT "'+name.replace('"','""')+'" NOT NULL',1)
 if line==');': table=None
 lines.append(line)
sql=normalize('\n'.join(lines))+'\n'+normalize((tmp/'seeds.raw').read_text())+'\n'+(tmp/'scheduler.sql').read_text()
catalog=hashlib.sha256((tmp/'catalog.json').read_bytes().removesuffix(b'\n')).hexdigest()
metadata=json.dumps({'catalog_sha256':catalog},separators=(',',':'))
upgrade=(root/'upgrade.sql').read_text()
transition=upgrade[upgrade.index('-- ocservia:step=001:checkpoint'):upgrade.index('-- ocservia:end-transition')]
history=hashlib.sha256(transition.encode()).hexdigest()
sql='-- ocservia:artifact=schema\n-- ocservia:format=1\n-- ocservia:engine=postgresql\n-- ocservia:epoch=2\n-- ocservia:revision=0\n-- ocservia:history-sha256='+history+'\n\n-- ocservia:step=001:baseline\n-- ocservia:metadata='+metadata+'\n'+sql+'-- ocservia:end-step\n'
(tmp/'schema.sql').write_text(sql)

PY
# Apply the final SQL directly in a separate database, independently of history.
# Compare all pg_dump schema semantics (including ACLs and routine security),
# plus all mandatory seeds. Only the scheduler's initialization timestamp varies.
psql -c 'CREATE DATABASE snapshot_equivalence' >/dev/null
psql -d snapshot_equivalence --single-transaction < "$tmp/schema.sql" >/dev/null
docker exec "$name" pg_dump -U ocservia_owner -d snapshot_equivalence --schema-only --no-owner --schema=public > "$tmp/equivalent-schema.raw"
docker exec "$name" pg_dump -U ocservia_owner -d snapshot_equivalence --data-only --no-owner --column-inserts --table=roles --table=upstream_sync_records --table=controller_schema_compatibility > "$tmp/equivalent-seeds.raw"
psql -d snapshot_equivalence -Atc "SELECT format('INSERT INTO public.scheduler_leadership(id,instance_id,incarnation,epoch,lease_until) VALUES(%s,%L,%s,%s,%L);',id,instance_id,incarnation,epoch,lease_until) FROM scheduler_leadership ORDER BY id" > "$tmp/equivalent-scheduler.sql"
for db in snapshot snapshot_equivalence; do
 psql -d "$db" -Atc "SELECT coalesce(json_agg(json_build_object('table',c.relname,'name',x.conname,'definition',pg_get_constraintdef(x.oid,true)) ORDER BY c.relname,x.conname),'[]') FROM pg_constraint x JOIN pg_class c ON c.oid=x.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE x.contype='c' AND n.nspname='public'" > "$tmp/$db-checks.json"
done
python3 - "$tmp" <<'PY_COMPARE'
import difflib,json,pathlib,re,sys
root=pathlib.Path(sys.argv[1])
def normalize(s,db):
 checks={(r['table'],r['name']):r['definition'] for r in json.loads((root/(db+'-checks.json')).read_text())}
 lines=[];table=None
 for line in s.splitlines():
  if line.startswith('--') or line.startswith('\\'): continue
  m=re.match(r'CREATE TABLE public\.(\w+) \(',line)
  if m: table=m[1]
  m=re.match(r'    CONSTRAINT (\w+) CHECK ',line)
  if table and m:
   # pg_dump reparses nested AND into n-ary AND; the server's pretty
   # deparser canonicalizes only this formatting, retaining every clause.
   line='    CONSTRAINT '+m[1]+' '+checks[table,m[1]]
  if line==');':table=None
  lines.append(line.rstrip())
 return re.sub(r'\n{3,}','\n\n','\n'.join(lines)).strip()
for name in ['schema.raw','seeds.raw','scheduler.sql']:
 before=normalize((root/name).read_text(),'snapshot')
 after=normalize((root/('equivalent-'+name)).read_text(),'snapshot_equivalence')
 if before!=after:
  print(''.join(difflib.unified_diff(before.splitlines(True),after.splitlines(True))))
  raise SystemExit('released checkpoint upgrade and fresh schema differ: '+name)

PY_COMPARE
for file in schema.sql; do
 if [[ "$mode" == generate ]]; then cp "$tmp/$file" "$ROOT/control-plane/migrations/$file"; else diff -u "$ROOT/control-plane/migrations/$file" "$tmp/$file"; fi
done
if [[ "$mode" == generate ]]; then
 python3 - "$ROOT/control-plane/migrations" <<'PY_BASELINE'
import hashlib,pathlib,re,sys
root=pathlib.Path(sys.argv[1]);upgrade=root/'upgrade.sql'
digest=hashlib.sha256((root/'schema.sql').read_bytes()).hexdigest()
upgrade.write_text(re.sub(r'(-- ocservia:baseline=\{"checksum":")[a-f0-9]{64}',r'\g<1>'+digest,upgrade.read_text(),count=1))
PY_BASELINE
fi
echo 'PostgreSQL bounded artifacts: pinned v1.2.0 upgrade and fresh schema/ACL/seed equivalence PASS'
