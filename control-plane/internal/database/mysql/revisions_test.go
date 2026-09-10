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
		"mysql/000005.json":            "0446b47cc83175873680e140e1bd20dd284a89fcd2907a79a81573152ba80ad1",
		"mariadb/000005.json":          "3483d0f381fd4b1b18235eef1e21a681c96b39c227f91487da09d696c281da77",
		"mysql/000006.json":            "8519e36210c309f4e070199db7cee6d7c4e6838cacd6e58a9239b46bcc2ba365",
		"mariadb/000006.json":          "63d54a9b8ef56ce50d7a7581f00f0a05e4acf6d341f5e5ba5aa06d5593bcd540",
		"mysql/000007.json":            "c7fbd4c7ea5ba8e9ded5551b499cf80018df9fb1e45b620b107c555d191fd316",
		"mariadb/000007.json":          "83cf9c6f0e98d59199ee9a7d4a364a0d0ba6e7e1787b8d4d75640ac17360515f",
		"mysql/000008.json":            "1fa7a5b3549acb3d9aa517ef2c0c0af24fb72fdffa3ff4baf18d71dc8e6700f1",
		"mariadb/000008.json":          "2f2beda52c03c2c6245ff1d5258c090875d42bf5b101fe71d35cdcb986df6803",
		"mysql/000009.json":            "626e74bfdad3d05b984102994a37409baab5b0e7e8da0f3d8195daa382657f42",
		"mariadb/000009.json":          "ae588b487092ce6ed4ca81b4042cb424551c93134d443da86899f8279684b6cf",
		"mysql/000010.json":            "7f019c127107f9ae812f89ecc988864a611eabe69ea62514539d5f0421757e89",
		"mariadb/000010.json":          "858d2c27efdfe414e3dae575c545e12fc9457769660538b358ad3456a6614217",
		"mysql/000011.json":            "73d40ddefb81fbbe214cc63620059c2eaf41ad1f1236018a6ab81d5fed0ba874",
		"mariadb/000011.json":          "ee5c2d393970f159a14f15346427cda70ccdc8d596ee6fccae2284139b3f7eed",
		"mysql/000012.json":            "6e1295711e6eb183640107aedc81ad1d49576e0cfa23e7d0d6fd0ab67d49e3a3",
		"mariadb/000012.json":          "5e7a4e21b2066147b5b17ba126b0fbad775f98044322870075c33e8bad6803bf",
		"mysql/000013.json":            "450336e37b4012ebb658b64bfa09991d3a03cece106d94b811c3bfd71e6185ad",
		"mariadb/000013.json":          "5eaf4f0b04e7a64af8f207520d272d38da8003b68558c9d87afffa9b3e0525d4",
		"mysql/000014.json":            "0a0469e93f519cdbf794052872467cfd4d98f0f428668761488cb7f1bbbe2fec",
		"mariadb/000014.json":          "f711fdd88d076ff7684848a4b69220a9d3ea1f955278b4bf33efc730607335fa",
		"mysql/000015.json":            "754244efb4e71292ceb52e572418d5ad1aeb50aa22e67eba0df44f3b4ab7a8b9",
		"mariadb/000015.json":          "2ed7b72dd6a0c1e933d96ef8265296af5ef67e2f6a0d21c0b7bc8551383b2082",
		"mysql/000016.json":            "6ec788471bf23db727b485846e1e6bcc81a8f7355a093e1a20c42382ed292de7",
		"mariadb/000016.json":          "90cbdad846a3b33e17a7333e004c4c19d1c3e942ecabd1cb509e0e74f66bb890",
		"mysql/000017.json":            "51e265a1828a5406f4967dae369cd4ab21fd136bf07b5874451dfa2a38c337eb",
		"mariadb/000017.json":          "79057ca8acf58fe57f38a7598d0ab7f85c53bbbc0ddf5e310ced68d174ae1460",
		"mysql/000018.json":            "a8b4a12007b70de2dbdbe322404590903871a8755c4dfd3dc84f825ea07c6477",
		"mariadb/000018.json":          "524030c120686414793db78cf03a22db74bd14c68e5e670d7b965ccc39d2cbd8",
		"mysql/000019.json":            "5ce9fd40f0fa8526087eac2a843bf2c076f8f8d8719b129c28c59e68904a5326",
		"mariadb/000019.json":          "dd801b20f75f0914da52f7ef1e3391172d8fda7d33a6869d2e14bcc4ccc2382e",
		"mysql/000020.json":            "9410b416129570d9637e75c8ab48090b970d208fde75baad64ca4a0e52fba05f",
		"mariadb/000020.json":          "578bac31feed6a42d08304c667a089d815a4151b158b70ecb520565934d15263",
		"mysql/000021.json":            "6ed9aebef346ceefd9eb1ec9d1fe4a390c5d189b7666c76ab9e1ac4b9bbb1726",
		"mariadb/000021.json":          "4dee90d40568df0fb32d40d9aece8861458cb8f36091c4622b863b22dc0b57eb",
		"mysql/000022.json":            "049b883526489e663c35e22858a0d1e873bfa4c5c83a70a79ece89cc9c23eefa",
		"mariadb/000022.json":          "9c458d72d11e52909df216f0dfe54bff33f6845f2e9a24c028b1b55a339edfcd",
		"mysql/000023.json":            "f8b072a37665191671edc7014daa381184f90fd14c1fc073ba92de030722b3ef",
		"mariadb/000023.json":          "1498bc98fe7699fc70ad564c67bca1e189dd44413bff2c47ddfbee53eb151c5d",
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
