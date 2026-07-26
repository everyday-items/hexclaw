package migrate

// K12PrintArtifactRendersV45 adds the one-to-one immutable PDF projection of
// the V25 printable Artifact. The Artifact remains the sole public identity;
// GenericPrintJob continues to hold only native-print receipt state.
var K12PrintArtifactRendersV45 = Migration{
	Version:     45,
	Description: "BUG-20260726 通用打印 Artifact 一对一不可变 PDF 投影",
	SQL:         K12PrintArtifactRendersV45DDL,
}

const K12PrintArtifactRendersV45DDL = `
CREATE TABLE IF NOT EXISTS k12_print_artifact_renders (
    artifact_id             TEXT    PRIMARY KEY
                                    REFERENCES k12_print_artifacts(artifact_id) ON DELETE CASCADE,
    format                  TEXT    NOT NULL CHECK(format = 'pdf'),
    render_contract_version TEXT    NOT NULL
                                    CHECK(length(trim(render_contract_version)) BETWEEN 1 AND 128),
    content_type            TEXT    NOT NULL CHECK(content_type = 'application/pdf'),
    byte_digest             TEXT    NOT NULL CHECK(length(byte_digest) = 64),
    byte_size               INTEGER NOT NULL CHECK(byte_size BETWEEN 1 AND 31457280),
    payload                 BLOB    NOT NULL CHECK(length(payload) = byte_size),
    created_at              INTEGER NOT NULL CHECK(created_at > 0)
);

CREATE TRIGGER IF NOT EXISTS trg_k12_print_artifact_renders_immutable
BEFORE UPDATE ON k12_print_artifact_renders
BEGIN
    SELECT RAISE(ABORT, 'k12 print artifact render is immutable');
END;
`
