package rest

import (
	"net/http"
	"testing"
)

// TestCategoryFlow covers local grouping categories end-to-end: create
// (type 4), move a channel under it (PATCH parent_id), see both in the
// guild channel assembly, ungroup via null, delete the category and
// confirm the child survived ungrouped.
func TestCategoryFlow(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	// Join a network, then create one text channel.
	rec, out := do(t, h, "POST", "/api/v9/guilds", token, map[string]any{
		"name": "ircs://irc.libera.chat:6697/#go?name=Libera",
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("join: %d %v", rec.Code, out)
	}
	guildID, _ := out["id"].(string)

	rec, ch := do(t, h, "POST", "/api/v9/guilds/"+guildID+"/channels", token, map[string]any{
		"name": "general",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create channel: %d %v", rec.Code, ch)
	}
	chID, _ := ch["id"].(string)

	// Create the category.
	rec, cat := do(t, h, "POST", "/api/v9/guilds/"+guildID+"/channels", token, map[string]any{
		"name": "Text",
		"type": 4,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create category: %d %v", rec.Code, cat)
	}
	if cat["type"].(float64) != 4 {
		t.Fatalf("category type: %v", cat["type"])
	}
	catID, _ := cat["id"].(string)
	if catID == "" {
		t.Fatal("no category id")
	}

	assemble := func() (text, category map[string]any) {
		t.Helper()
		rec, g := do(t, h, "GET", "/api/v9/guilds/"+guildID, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("guild detail: %d", rec.Code)
		}
		for _, c := range g["channels"].([]any) {
			cm := c.(map[string]any)
			if cm["id"] == chID {
				text = cm
			}
			if cm["id"] == catID {
				category = cm
			}
		}
		return
	}

	// Move the channel under the category; the echo and the assembly
	// both carry the parent.
	rec, moved := do(t, h, "PATCH", "/api/v9/channels/"+chID, token, map[string]any{"parent_id": catID})
	if rec.Code != http.StatusOK {
		t.Fatalf("move: %d %v", rec.Code, moved)
	}
	if moved["parent_id"] != catID {
		t.Fatalf("echo parent: %v", moved["parent_id"])
	}
	text, category := assemble()
	if category == nil || category["type"].(float64) != 4 {
		t.Fatalf("assembly category: %v", category)
	}
	if text == nil || text["parent_id"] != catID {
		t.Fatalf("assembly text/parent: %v", text)
	}

	// Ungroup via null parent.
	rec, _ = do(t, h, "PATCH", "/api/v9/channels/"+chID, token, map[string]any{"parent_id": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("ungroup: %d", rec.Code)
	}
	if text, _ = assemble(); text["parent_id"] != nil {
		t.Fatalf("ungroup parent: %v", text["parent_id"])
	}

	// Delete the category: it disappears, the child survives ungrouped.
	rec, _ = do(t, h, "DELETE", "/api/v9/channels/"+catID, token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete category: %d", rec.Code)
	}
	text, category = assemble()
	if category != nil {
		t.Fatal("category survived delete")
	}
	if text == nil || text["parent_id"] != nil {
		t.Fatalf("child after category delete: %v", text)
	}
}

// TestCategoryDragMove covers the sidebar drag route: the batch positions
// PATCH (array of {id, parent_id, position}) groups the channel.
func TestCategoryDragMove(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	rec, out := do(t, h, "POST", "/api/v9/guilds", token, map[string]any{
		"name": "ircs://irc.libera.chat:6697/#go?name=Libera",
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("join: %d %v", rec.Code, out)
	}
	guildID, _ := out["id"].(string)
	rec, ch := do(t, h, "POST", "/api/v9/guilds/"+guildID+"/channels", token, map[string]any{"name": "general"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create channel: %d %v", rec.Code, ch)
	}
	chID, _ := ch["id"].(string)
	rec, cat := do(t, h, "POST", "/api/v9/guilds/"+guildID+"/channels", token, map[string]any{"name": "Deep", "type": 4})
	if rec.Code != http.StatusOK {
		t.Fatalf("create category: %d %v", rec.Code, cat)
	}
	catID, _ := cat["id"].(string)

	// The drag: batch array, 204 expected.
	rec, _ = doAny(t, h, "PATCH", "/api/v9/guilds/"+guildID+"/channels", token, []any{
		map[string]any{"id": chID, "parent_id": catID, "position": 0, "lock_permissions": false},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("drag patch: %d", rec.Code)
	}
	rec, g := do(t, h, "GET", "/api/v9/guilds/"+guildID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("guild detail: %d", rec.Code)
	}
	for _, c := range g["channels"].([]any) {
		cm := c.(map[string]any)
		if cm["id"] == chID && cm["parent_id"] != catID {
			t.Fatalf("drag did not group: %v", cm)
		}
	}
}
