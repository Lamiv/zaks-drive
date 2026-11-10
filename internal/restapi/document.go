package restapi

import (
	"net/url"
	"time"

	"github.com/Lamiv/zaks-drive/internal/storage"
)

// Document is the REST representation of a SharePoint drive item.
type Document struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Repository string    `json:"repository"`
	Path       string    `json:"path"`
	IsFolder   bool      `json:"isFolder"`
	Size       int64     `json:"size"`
	MimeType   string    `json:"mimeType,omitempty"`
	ETag       string    `json:"eTag,omitempty"`
	ContentTag string    `json:"contentTag,omitempty"`
	ChildCount *int      `json:"childCount,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`
	CreatedBy  string    `json:"createdBy,omitempty"`
	ModifiedBy string    `json:"modifiedBy,omitempty"`
	WebURL     string    `json:"webUrl,omitempty"`
	Hashes     *Hashes   `json:"hashes,omitempty"`
	Links      Links     `json:"links"`
}

type Hashes struct {
	QuickXor string `json:"quickXor,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

type Links struct {
	Self      string `json:"self"`
	Content   string `json:"content,omitempty"`
	Thumbnail string `json:"thumbnail,omitempty"`
}

func toDocument(repo *storage.Repository, it *storage.Item) Document {
	rel := it.Path
	base := prefix + "/repositories/" + url.PathEscape(repo.ID) + "/documents/" + url.PathEscape(it.ID)
	d := Document{
		ID:         it.ID,
		Name:       it.Name,
		Repository: repo.ID,
		Path:       rel,
		Size:       it.Size,
		ETag:       it.ETag,
		ContentTag: it.CTag,
		CreatedAt:  it.Created,
		ModifiedAt: it.Modified,
		CreatedBy:  it.CreatedBy,
		ModifiedBy: it.ModifiedBy,
		WebURL:     it.WebURL,
		Links:      Links{Self: base},
	}
	if it.Folder {
		d.IsFolder = true
		n := it.ChildCount
		d.ChildCount = &n
	}
	if it.File {
		d.MimeType = it.MimeType
		d.Links.Content = base + "/content"
		d.Links.Thumbnail = base + "/thumbnail"
		if it.QuickXorHash != "" || it.SHA256Hash != "" {
			d.Hashes = &Hashes{QuickXor: it.QuickXorHash, SHA256: it.SHA256Hash}
		}
	}
	return d
}
