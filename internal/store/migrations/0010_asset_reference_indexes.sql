-- 0010_asset_reference_indexes.sql — point-lookup indexes on every catalogue
-- column that names a content-addressed asset-store blob
-- (ADR-0041 "Reference check before delete").
--
-- Assets are content-addressed and SHARED: the same SHA-256 can name a blob
-- the speech cache generated AND a blob a catalogue row still names (an
-- approved voice reference, an audition clip, a character portrait, an
-- imported source). EvictTransient must therefore ask, per evicted blob,
-- whether any other row still names it before it may delete. That question is
-- one UNION over every referencing column (Store.AssetReferenced), and each
-- branch must be a point lookup, not a table scan. Two of the five referencing
-- columns were already indexed at creation; the other three get their index
-- here.
--
--   source_blobs.sha256                        PRIMARY KEY (0001)
--   sources.content_sha256                     sources_content_sha256_idx (0001)
--   characters.portrait_asset_id               ← characters_portrait_asset_idx (here)
--   voice_profiles.canonical_reference_asset_id ← voice_profiles_reference_asset_idx (here)
--   voice_auditions.asset_sha256               ← voice_auditions_asset_idx (here)
--
-- Columns that mention hashes only inside JSON (characters.visual_descriptors,
-- scenes.visual_anchors, regions.mask_asset_ids, messages.attribution_evidence)
-- are evidence pointers — "refer by hash, verify on read" (0002) — not
-- ownership references, and are deliberately not indexed or checked.
--
-- Indexes only: no column, no default, no data change. The query is a pure
-- read, so nothing written before this migration changes shape; existing
-- databases pick the indexes up on the next migrate (backward compatible by
-- construction).

CREATE INDEX characters_portrait_asset_idx ON characters (portrait_asset_id);
CREATE INDEX voice_profiles_reference_asset_idx ON voice_profiles (canonical_reference_asset_id);
CREATE INDEX voice_auditions_asset_idx ON voice_auditions (asset_sha256);
