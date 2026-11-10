package storage

import (
	"testing"

	"github.com/Lamiv/zaks-drive/internal/graph"
)

func TestCleanPath(t *testing.T) {
	ok := map[string]string{"": "", "/a/b/": "a/b", `a\b`: "a/b", "BUS2081/4711": "BUS2081/4711"}
	for in, want := range ok {
		if got, err := CleanPath(in); err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"..", "a/../b", "a//b", "a/./b", "a:b", "x?", "trailing.", " lead"} {
		if _, err := CleanPath(bad); err == nil {
			t.Errorf("CleanPath(%q) accepted", bad)
		}
	}
}

func TestSharePointBackendRel(t *testing.T) {
	r := &sharePointBackend{root: "SAP/DMS"}
	cases := map[string]struct {
		rel string
		ok  bool
	}{
		"/SAP/DMS/a.pdf":      {"a.pdf", true},
		"/SAP/DMS":            {"", true},
		"/SAP/DMSX/a.pdf":     {"", false},
		"/HR/a.pdf":           {"", false},
		"/SAP/DMS/x/y/z.docx": {"x/y/z.docx", true},
	}
	for in, want := range cases {
		rel, ok := r.rel(in)
		if rel != want.rel || ok != want.ok {
			t.Errorf("Rel(%q) = %q,%v want %q,%v", in, rel, ok, want.rel, want.ok)
		}
	}
}

func TestSharePointBackendAdaptsItems(t *testing.T) {
	b := &sharePointBackend{root: "SAP/DMS"}
	d := &graph.DriveItem{ID: "1", Name: "a.pdf", Size: 7, ETag: "e", CTag: "c", DownloadURL: "https://dl",
		CreatedBy: &graph.IdentitySet{User: &graph.Identity{DisplayName: "Ann"}}}
	d.ParentReference = &struct {
		DriveID string `json:"driveId"`
		ID      string `json:"id"`
		Path    string `json:"path"`
	}{Path: "/drives/x/root:/SAP/DMS/sub"}
	d.File = &struct {
		MimeType string `json:"mimeType"`
		Hashes   *struct {
			QuickXorHash string `json:"quickXorHash,omitempty"`
			SHA256Hash   string `json:"sha256Hash,omitempty"`
		} `json:"hashes,omitempty"`
	}{MimeType: "application/pdf"}

	it, inside := b.toItem(d)
	if !inside || it.Path != "sub/a.pdf" || !it.PathKnown || !it.File || it.Folder || it.MimeType != "application/pdf" ||
		it.DirectURL != "https://dl" || it.CreatedBy != "Ann" || it.CTag != "c" {
		t.Fatalf("unexpected item %+v (inside=%v)", it, inside)
	}

	d.ParentReference.Path = "/drives/x/root:/HR"
	if _, inside := b.toItem(d); inside {
		t.Fatal("an item in another folder must be outside the repository")
	}
}

func TestSharePointBackendPageDropsItemsOutsideRoot(t *testing.T) {
	b := &sharePointBackend{root: "SAP/DMS"}
	mk := func(id, parent string) graph.DriveItem {
		d := graph.DriveItem{ID: id, Name: id}
		if parent != "" {
			d.ParentReference = &struct {
				DriveID string `json:"driveId"`
				ID      string `json:"id"`
				Path    string `json:"path"`
			}{Path: parent}
		}
		return d
	}
	pg := b.page(&graph.Page{
		Items:    []graph.DriveItem{mk("in", "/drives/x/root:/SAP/DMS"), mk("out", "/drives/x/root:/HR"), mk("nowhere", "")},
		NextLink: "next",
	})
	var got []string
	for _, it := range pg.Items {
		got = append(got, it.ID)
	}
	if len(got) != 2 || got[0] != "in" || got[1] != "nowhere" || pg.Next != "next" {
		t.Fatalf("items %v next %q; want [in nowhere] and next", got, pg.Next)
	}
}

func TestRepositoryHidesReservedItems(t *testing.T) {
	r := &Repository{reserved: []string{"ContentServer"}}
	if r.visible(&Item{Path: "ContentServer/Z1/a", PathKnown: true}) {
		t.Error("an item in a reserved folder must be hidden")
	}
	if !r.visible(&Item{Path: "Other/a", PathKnown: true}) || !r.visible(&Item{}) {
		t.Error("ordinary items and items of unknown location must stay visible")
	}
}
