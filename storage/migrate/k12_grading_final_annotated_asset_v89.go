package migrate

// K12GradingFinalAnnotatedAssetV89 把批注图的 owner-scoped PageAsset 身份
// 与最终批改产物一对一冻结，不创建第二份图片存储。
var K12GradingFinalAnnotatedAssetV89 = Migration{
	Version:     89,
	Description: "K12 durable annotated grading final artifact",
	SQL:         k12GradingFinalAnnotatedAssetV89DDL,
}

const k12GradingFinalAnnotatedAssetV89DDL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_grading_final_artifacts_agent_identity
    ON k12_grading_final_artifacts(agent_name,artifact_id);

CREATE TABLE IF NOT EXISTS k12_grading_final_artifact_assets (
    artifact_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL CHECK(length(trim(agent_name)) > 0),
    annotated_asset_owner_scope TEXT NOT NULL
        CHECK(length(trim(annotated_asset_owner_scope)) > 0),
    annotated_asset_id TEXT NOT NULL CHECK(length(trim(annotated_asset_id)) > 0),
    annotated_mime TEXT NOT NULL
        CHECK(annotated_mime IN ('image/png','image/jpeg','image/gif','image/webp')),
    annotated_digest TEXT NOT NULL
        CHECK(
            length(annotated_digest) = 64 AND
            annotated_digest NOT GLOB '*[^0-9a-f]*'
        ),
    original_source_digest TEXT NOT NULL
        CHECK(
            length(original_source_digest) = 64 AND
            original_source_digest NOT GLOB '*[^0-9a-f]*'
        ),
    FOREIGN KEY(agent_name,artifact_id)
        REFERENCES k12_grading_final_artifacts(agent_name,artifact_id) ON DELETE CASCADE,
    FOREIGN KEY(annotated_asset_owner_scope,annotated_asset_id)
        REFERENCES k12_page_assets(owner_scope,page_asset_id),
    CHECK(
        annotated_asset_id = 'asset://' || agent_name || '/' || annotated_digest ||
            CASE annotated_mime
                WHEN 'image/png' THEN '.png'
                WHEN 'image/jpeg' THEN '.jpg'
                WHEN 'image/gif' THEN '.gif'
                WHEN 'image/webp' THEN '.webp'
            END
    )
);

CREATE INDEX IF NOT EXISTS idx_k12_grading_final_artifact_assets_scope
    ON k12_grading_final_artifact_assets(
        annotated_asset_owner_scope,agent_name,annotated_asset_id
    );

CREATE TRIGGER IF NOT EXISTS k12_grading_final_artifact_asset_immutable
BEFORE UPDATE ON k12_grading_final_artifact_assets
BEGIN
    SELECT RAISE(ABORT, 'grading final annotated asset identity is immutable');
END;
`
