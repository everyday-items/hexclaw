package k12

import "testing"

func completeSubjectTextbooks() SubjectTextbooks {
	return SubjectTextbooks{
		Math:                  "人教版",
		Chinese:               "统编版",
		English:               "外研版",
		Science:               "教科版",
		InformationTechnology: "浙教版",
		Art:                   "人美版",
	}
}

func TestSubjectTextbooksCanonicalMetadataContract(t *testing.T) {
	t.Run("canonical math wins and scalar is derived", func(t *testing.T) {
		meta := map[string]string{
			MetaKeyTextbook:     "旧镜像",
			MetaKeyTextbookMath: "北师大版",
		}
		profile := ProfileFromMeta(meta)
		if profile.SubjectTextbooks.Math != "北师大版" ||
			profile.TextbookEdition != "北师大版" {
			t.Fatalf("profile=%+v", profile)
		}
	})

	t.Run("legacy scalar only backfills missing canonical math", func(t *testing.T) {
		profile := ProfileFromMeta(map[string]string{MetaKeyTextbook: "人教版"})
		if profile.SubjectTextbooks.Math != "人教版" ||
			profile.TextbookEdition != "人教版" {
			t.Fatalf("profile=%+v", profile)
		}
		if profile.SubjectTextbooks.Chinese != "" {
			t.Fatalf("legacy math leaked to another subject: %+v", profile.SubjectTextbooks)
		}
	})

	t.Run("full write persists six canonical values and math mirror", func(t *testing.T) {
		original := map[string]string{"unrelated": "keep"}
		textbooks := completeSubjectTextbooks()
		got := ApplyProfileToMeta(original, ChildProfile{
			ChildName: "明明", GradeTerm: "五年级下",
			SubjectTextbooks: textbooks, TextbookEdition: "不得成为独立事实",
		})
		want := map[string]string{
			MetaKeyTextbookMath:                  textbooks.Math,
			MetaKeyTextbookChinese:               textbooks.Chinese,
			MetaKeyTextbookEnglish:               textbooks.English,
			MetaKeyTextbookScience:               textbooks.Science,
			MetaKeyTextbookInformationTechnology: textbooks.InformationTechnology,
			MetaKeyTextbookArt:                   textbooks.Art,
		}
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("metadata[%q]=%q want %q; all=%v", key, got[key], value, got)
			}
		}
		if got[MetaKeyTextbook] != textbooks.Math || got["unrelated"] != "keep" {
			t.Fatalf("derived mirror or unrelated metadata drifted: %v", got)
		}
		if _, changed := original[MetaKeyTextbook]; changed {
			t.Fatalf("ApplyProfileToMeta mutated input: %v", original)
		}
	})

	t.Run("legacy scalar is a canonical math patch", func(t *testing.T) {
		before := ApplyProfileToMeta(nil, ChildProfile{
			SubjectTextbooks: completeSubjectTextbooks(),
		})
		got := ApplyProfileToMeta(before, ChildProfile{TextbookEdition: "北师大版"})
		if got[MetaKeyTextbookMath] != "北师大版" ||
			got[MetaKeyTextbook] != "北师大版" {
			t.Fatalf("math patch/mirror=%v", got)
		}
		if got[MetaKeyTextbookChinese] != "统编版" ||
			got[MetaKeyTextbookArt] != "人美版" {
			t.Fatalf("legacy math patch changed another subject: %v", got)
		}
	})
}
