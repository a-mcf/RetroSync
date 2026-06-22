-- Roll back the save-version recovery net. save_versions references save_blobs,
-- so drop save_versions first. (Dev-only re-model; no data is preserved.)

DROP INDEX IF EXISTS save_versions_sync_node_seq_idx;
DROP TABLE IF EXISTS save_versions;
DROP TABLE IF EXISTS save_blobs;
