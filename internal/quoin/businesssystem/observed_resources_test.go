package businesssystem

import (
	"context"
	"testing"
)

func TestListObservedResourcesAllowsUnpublishedBrowserIdentitySystem(t *testing.T) {
	h := newHarness(t)
	// Browser Identity may be configured before an inspection configuration is
	// published. Its resources list is still loaded by the same detail page.
	h.mustUpload(t, validSystemYAML, 1, "cmd-resource-list-upload-0001")
	current := true
	if _, _, err := h.systems.ListObservedResources(context.Background(), "payments", &current, 0, 100); err != nil {
		t.Fatalf("list resources for an unpublished system: %v", err)
	}
}
