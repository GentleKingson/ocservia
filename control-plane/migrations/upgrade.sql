-- ocservia:artifact=upgrade
-- ocservia:format=1
-- ocservia:engine=postgresql
-- ocservia:epoch=2
-- ocservia:baseline={"checksum":"a870fd98925aed844a62cab28b034a86ee64b9ab8afa942ef5b630233255d305","steps":1,"metadata":{"catalog_sha256":"096ad0fc5ad94f3d8c93fb94ffb9935b65a40bab31bc15cc84b7961d7755800c"}}
-- ocservia:previous-checkpoint-epoch=1
-- ocservia:previous-checkpoint-revision=0
-- ocservia:previous-checkpoint-ref=v1.2.0@169102557cd610847c9f6ac2083336cdcf82c483
-- ocservia:previous-checkpoint-receipt={"checksum":"d837335f22c70858f4e2a5277332e478ecd6032e7b55f57d6512484bfab6f172","steps":1,"metadata":{"catalog_sha256":"a69f2e270c62191be30309d10fffcacc27b960487784a984c18ed69081c84a3b"}}

-- ocservia:transition=1:0->2:0
-- ocservia:step=001:checkpoint
SELECT 1;
-- ocservia:end-step
-- ocservia:step=002:legacy_cleanup
-- ocservia:metadata={"catalog_sha256":"096ad0fc5ad94f3d8c93fb94ffb9935b65a40bab31bc15cc84b7961d7755800c"}
DROP TABLE public.schema_migrations, public.schema_snapshot_origin;
DELETE FROM public.schema_revisions WHERE epoch=1;
-- ocservia:end-step
-- ocservia:end-transition
