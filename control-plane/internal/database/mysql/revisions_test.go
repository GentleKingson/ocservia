package mysql

import "testing"

func TestPublishedDraftArtifactsAreImmutable(t *testing.T) {
	for path, want := range map[string]string{
		"history/f6cd0e0/mysql.json": "9ad53381a9a75ae5e0594e4c958c326d1718914e1263964edee2679856f1a2c2",
		"mysql/manifest.json":        "f04cb4c7ad10885d4dad1d90c8bc92789fa0bf403e5e3316c384a1a8d233ea8f",
		"mysql/000002.json":          "e5887aeed3c802248f5369d0b3f2ed52524b61af94a04e311edca0b4399dcea3",
		"mysql/000003.json":          "ef17a7cb819042e989c2aec17283607ab44ae4138b7301d7af863944f4165c29",
		"mysql/000004.json":          "d67c7a44b41c716c6ff19c5ea49b26febb842f8869a8f2a6d1d696fb5aa5e11d",
		"mysql/000005.json":          "0446b47cc83175873680e140e1bd20dd284a89fcd2907a79a81573152ba80ad1",
		"mysql/000006.json":          "8519e36210c309f4e070199db7cee6d7c4e6838cacd6e58a9239b46bcc2ba365",
		"mysql/000007.json":          "c7fbd4c7ea5ba8e9ded5551b499cf80018df9fb1e45b620b107c555d191fd316",
		"mysql/000008.json":          "1fa7a5b3549acb3d9aa517ef2c0c0af24fb72fdffa3ff4baf18d71dc8e6700f1",
		"mysql/000009.json":          "626e74bfdad3d05b984102994a37409baab5b0e7e8da0f3d8195daa382657f42",
		"mysql/000010.json":          "7f019c127107f9ae812f89ecc988864a611eabe69ea62514539d5f0421757e89",
		"mysql/000011.json":          "73d40ddefb81fbbe214cc63620059c2eaf41ad1f1236018a6ab81d5fed0ba874",
		"mysql/000012.json":          "6e1295711e6eb183640107aedc81ad1d49576e0cfa23e7d0d6fd0ab67d49e3a3",
		"mysql/000013.json":          "450336e37b4012ebb658b64bfa09991d3a03cece106d94b811c3bfd71e6185ad",
		"mysql/000014.json":          "0a0469e93f519cdbf794052872467cfd4d98f0f428668761488cb7f1bbbe2fec",
		"mysql/000015.json":          "754244efb4e71292ceb52e572418d5ad1aeb50aa22e67eba0df44f3b4ab7a8b9",
		"mysql/000016.json":          "6ec788471bf23db727b485846e1e6bcc81a8f7355a093e1a20c42382ed292de7",
		"mysql/000017.json":          "51e265a1828a5406f4967dae369cd4ab21fd136bf07b5874451dfa2a38c337eb",
		"mysql/000018.json":          "a8b4a12007b70de2dbdbe322404590903871a8755c4dfd3dc84f825ea07c6477",
		"mysql/000019.json":          "5ce9fd40f0fa8526087eac2a843bf2c076f8f8d8719b129c28c59e68904a5326",
		"mysql/000020.json":          "9410b416129570d9637e75c8ab48090b970d208fde75baad64ca4a0e52fba05f",
		"mysql/000021.json":          "6ed9aebef346ceefd9eb1ec9d1fe4a390c5d189b7666c76ab9e1ac4b9bbb1726",
		"mysql/000022.json":          "049b883526489e663c35e22858a0d1e873bfa4c5c83a70a79ece89cc9c23eefa",
		"mysql/000023.json":          "f8b072a37665191671edc7014daa381184f90fd14c1fc073ba92de030722b3ef",
	} {
		data, err := manifests.ReadFile(path)
		if err != nil || digest(data) != want {
			t.Fatalf("published artifact changed: %s", path)
		}
	}
	for _, engine := range []Engine{MySQL} {
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
