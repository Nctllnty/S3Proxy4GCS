package translate

import (
	"log/slog"

	"cloud.google.com/go/storage"
)

// TranslateS3ToGCSContexts computes the GCS Object Contexts patch that
// mirrors the supplied S3 Tagging XML document.
//
// Semantics:
//   - Every key in `existing.Custom` that is NOT present in the new TagSet
//     is marked with Delete=true so GCS removes it.
//   - Every tag in the new TagSet is written as an upsert
//     (Value populated, Delete=false). If the same key existed before,
//     the upsert supersedes the delete marker.
//
// The returned *ObjectContexts is safe to assign directly to
// ObjectAttrsToUpdate.Contexts.
func TranslateS3ToGCSContexts(s3Cfg Tagging, existing *storage.ObjectContexts) *storage.ObjectContexts {
	custom := make(map[string]storage.ObjectCustomContextPayload)

	// 1. Mark every existing context key for deletion first.
	if existing != nil {
		for k := range existing.Custom {
			custom[k] = storage.ObjectCustomContextPayload{Delete: true}
		}
	}

	// 2. Upsert new tags; this overrides any delete marker for the same key.
	for _, tag := range s3Cfg.TagSet {
		custom[tag.Key] = storage.ObjectCustomContextPayload{Value: tag.Value}
		slog.Debug("Tag translated to GCS Object Context", "key", tag.Key, "value", tag.Value)
	}

	return &storage.ObjectContexts{Custom: custom}
}

// TranslateGCSContextsToS3Tagging converts GCS Object Contexts back to an
// S3 Tagging XML document. A nil or empty Contexts yields an empty TagSet.
func TranslateGCSContextsToS3Tagging(ctx *storage.ObjectContexts) *Tagging {
	t := &Tagging{}
	if ctx == nil {
		return t
	}
	for k, v := range ctx.Custom {
		if k == "" {
			slog.Warn("Skipping GCS Object Context entry with empty key")
			continue
		}
		t.TagSet = append(t.TagSet, Tag{Key: k, Value: v.Value})
	}
	return t
}
