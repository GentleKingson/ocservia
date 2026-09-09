package mysql

import "testing"

func TestPublishedDraftArtifactsAreImmutable(t *testing.T) {
	for path, want := range map[string]string{
		"history/f6cd0e0/mysql.json":   "9ad53381a9a75ae5e0594e4c958c326d1718914e1263964edee2679856f1a2c2",
		"history/f6cd0e0/mariadb.json": "38a2195542671c49527493e96db1972c0151b1a0f95a8203f0a77f33fde7280c",
		"mysql/manifest.json":          "f04cb4c7ad10885d4dad1d90c8bc92789fa0bf403e5e3316c384a1a8d233ea8f",
		"mariadb/manifest.json":        "1d0c6d6b9bcd7b64dba44f74b0d96700258f2e60f217dfa207168904776606b6",
		"mysql/000002.json":            "e5887aeed3c802248f5369d0b3f2ed52524b61af94a04e311edca0b4399dcea3",
		"mariadb/000002.json":          "ff9a02df25c6f3f23ebf10ba94ee83b2cda4e67303868561624cd17b64c8bd1e",
		"mysql/000003.json":            "ef17a7cb819042e989c2aec17283607ab44ae4138b7301d7af863944f4165c29",
		"mariadb/000003.json":          "9a0353fd6b518b062f15bd5608a1777dd596175d028a36867a29c124e27f7e1d",
		"mysql/000004.json":            "d67c7a44b41c716c6ff19c5ea49b26febb842f8869a8f2a6d1d696fb5aa5e11d",
		"mariadb/000004.json":          "66cb14281fcfc42268260ab4a75826e1b3bc3047f1a6576f8d7206eef9a1b038",
	} {
		data, err := manifests.ReadFile(path)
		if err != nil || digest(data) != want {
			t.Fatalf("published artifact changed: %s", path)
		}
	}
	for _, engine := range []Engine{MySQL, MariaDB} {
		r, _, err := loadRevision(engine)
		if err != nil {
			t.Fatal(err)
		}
		for parent := range r.Parents {
			if _, _, err := baselineFor(engine, parent); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := baselineFor(engine, "unrecognized draft"); err == nil {
			t.Fatal("unknown history adopted")
		}
	}
}
