-- +goose Up
UPDATE public.operations AS op
SET request_hash = artifact.request_hash
FROM public.artifact_operations AS artifact
WHERE artifact.operation_id = op.id AND op.idempotency_key IS NOT NULL;

ALTER TABLE public.artifact_operations DROP COLUMN request_hash;
ALTER TABLE public.certificates DROP COLUMN issue_request_hash;
