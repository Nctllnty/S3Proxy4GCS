package translate

import (
	"encoding/xml"
	"testing"

	"cloud.google.com/go/storage"
)

func TestTranslateS3ToGCSContexts(t *testing.T) {
	xmlInput := `<?xml version="1.0" encoding="UTF-8"?>
<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
    <TagSet>
        <Tag>
            <Key>Project</Key>
            <Value>Foo</Value>
        </Tag>
        <Tag>
            <Key>Environment</Key>
            <Value>Dev</Value>
        </Tag>
    </TagSet>
</Tagging>`

	var s3Cfg Tagging
	if err := xml.Unmarshal([]byte(xmlInput), &s3Cfg); err != nil {
		t.Fatalf("Failed to unmarshal XML: %v", err)
	}

	existing := &storage.ObjectContexts{
		Custom: map[string]storage.ObjectCustomContextPayload{
			"OldKey": {Value: "OldValue"},
		},
	}

	out := TranslateS3ToGCSContexts(s3Cfg, existing)
	if out == nil {
		t.Fatalf("Expected non-nil ObjectContexts")
	}

	if p, ok := out.Custom["Project"]; !ok || p.Value != "Foo" || p.Delete {
		t.Errorf("Expected Project=Foo upsert, got %+v (ok=%v)", p, ok)
	}
	if e, ok := out.Custom["Environment"]; !ok || e.Value != "Dev" || e.Delete {
		t.Errorf("Expected Environment=Dev upsert, got %+v (ok=%v)", e, ok)
	}
	if o, ok := out.Custom["OldKey"]; !ok || !o.Delete {
		t.Errorf("Expected OldKey marked for deletion, got %+v (ok=%v)", o, ok)
	}
}

func TestTranslateS3ToGCSContextsOverrideExisting(t *testing.T) {
	// A tag with the same key as an existing context must upsert (not delete).
	s3Cfg := Tagging{TagSet: []Tag{{Key: "Project", Value: "Bar"}}}
	existing := &storage.ObjectContexts{
		Custom: map[string]storage.ObjectCustomContextPayload{
			"Project": {Value: "Foo"},
		},
	}

	out := TranslateS3ToGCSContexts(s3Cfg, existing)
	if out == nil {
		t.Fatalf("Expected non-nil ObjectContexts")
	}
	p, ok := out.Custom["Project"]
	if !ok {
		t.Fatalf("Project context missing from output")
	}
	if p.Delete {
		t.Errorf("Project should be upserted, got Delete=true")
	}
	if p.Value != "Bar" {
		t.Errorf("Project value should be overridden to Bar, got %q", p.Value)
	}
}

func TestTranslateS3ToGCSContextsNilExisting(t *testing.T) {
	s3Cfg := Tagging{TagSet: []Tag{{Key: "K", Value: "V"}}}
	out := TranslateS3ToGCSContexts(s3Cfg, nil)
	if out == nil || len(out.Custom) != 1 {
		t.Fatalf("Expected one entry, got %+v", out)
	}
	if out.Custom["K"].Value != "V" || out.Custom["K"].Delete {
		t.Errorf("Unexpected payload for K: %+v", out.Custom["K"])
	}
}

func TestTranslateGCSContextsToS3Tagging(t *testing.T) {
	ctx := &storage.ObjectContexts{
		Custom: map[string]storage.ObjectCustomContextPayload{
			"Project":     {Value: "Foo"},
			"Environment": {Value: "Dev"},
		},
	}

	tg := TranslateGCSContextsToS3Tagging(ctx)
	if tg == nil {
		t.Fatalf("Expected non-nil Tagging")
	}
	if len(tg.TagSet) != 2 {
		t.Fatalf("Expected 2 tags, got %d", len(tg.TagSet))
	}

	got := map[string]string{}
	for _, tag := range tg.TagSet {
		got[tag.Key] = tag.Value
	}
	if got["Project"] != "Foo" || got["Environment"] != "Dev" {
		t.Errorf("Unexpected TagSet content: %+v", got)
	}
}

func TestTranslateGCSContextsToS3TaggingNil(t *testing.T) {
	tg := TranslateGCSContextsToS3Tagging(nil)
	if tg == nil {
		t.Fatalf("Expected non-nil Tagging")
	}
	if len(tg.TagSet) != 0 {
		t.Errorf("Expected empty TagSet, got %d", len(tg.TagSet))
	}
}
