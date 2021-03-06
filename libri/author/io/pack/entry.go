package pack

import (
	"errors"
	"io"
	"time"

	"github.com/drausin/libri/libri/author/io/enc"
	"github.com/drausin/libri/libri/author/io/page"
	"github.com/drausin/libri/libri/author/io/print"
	"github.com/drausin/libri/libri/common/id"
	"github.com/drausin/libri/libri/common/storage"
	"github.com/drausin/libri/libri/librarian/api"
)

// EntryPacker creates entry documents from raw content.
type EntryPacker interface {
	// Pack prints pages from the content, encrypts their metadata, and binds them together
	// into an entry *api.Document.
	Pack(content io.Reader, mediaType string, keys *enc.Keys, authorPub []byte) (
		*api.Document, *api.Metadata, error)
}

// NewEntryPacker creates a new Packer instance.
func NewEntryPacker(
	params *print.Parameters,
	metadataEnc enc.MetadataEncrypter,
	docSL storage.DocumentStorerLoader,
) EntryPacker {
	pageS := page.NewStorerLoader(docSL)
	return &entryPacker{
		params:      params,
		metadataEnc: metadataEnc,
		printer:     print.NewPrinter(params, pageS),
		pageS:       pageS,
		docL:        docSL,
	}
}

type entryPacker struct {
	params      *print.Parameters
	metadataEnc enc.MetadataEncrypter
	printer     print.Printer
	pageS       page.Storer
	docL        storage.DocumentLoader
}

func (p *entryPacker) Pack(content io.Reader, mediaType string, keys *enc.Keys, authorPub []byte) (
	*api.Document, *api.Metadata, error) {

	pageKeys, metadata, err := p.printer.Print(content, mediaType, keys, authorPub)
	if err != nil {
		return nil, nil, err
	}
	// TODO (drausin) add additional metadata K/V here
	// - relative filepath
	// - file mode permissions
	encMetadata, err := p.metadataEnc.Encrypt(metadata, keys)
	if err != nil {
		return nil, nil, err
	}
	doc, err := newEntryDoc(authorPub, pageKeys, encMetadata, p.docL)
	return doc, metadata, err
}

// EntryUnpacker writes individual pages to the content io.Writer.
type EntryUnpacker interface {
	// Unpack extracts the individual pages from a document and stitches them together to write
	// to the content io.Writer.
	Unpack(content io.Writer, entry *api.Document, keys *enc.Keys) (*api.Metadata, error)
}

type entryUnpacker struct {
	params      *print.Parameters
	metadataDec enc.MetadataDecrypter
	scanner     print.Scanner
}

// NewEntryUnpacker creates a new EntryUnpacker with the given parameters, metadata decrypter, and
// storage.DocumentStorerLoader.
func NewEntryUnpacker(
	params *print.Parameters,
	metadataDec enc.MetadataDecrypter,
	docSL storage.DocumentStorerLoader,
) EntryUnpacker {
	pageL := page.NewStorerLoader(docSL)
	return &entryUnpacker{
		params:      params,
		metadataDec: metadataDec,
		scanner:     print.NewScanner(params, pageL),
	}
}

func (u *entryUnpacker) Unpack(content io.Writer, entry *api.Document, keys *enc.Keys) (
	*api.Metadata, error) {
	encMetadata, err := enc.NewEncryptedMetadata(
		entry.Contents.(*api.Document_Entry).Entry.MetadataCiphertext,
		entry.Contents.(*api.Document_Entry).Entry.MetadataCiphertextMac,
	)
	if err != nil {
		return nil, err
	}
	metadata, err := u.metadataDec.Decrypt(encMetadata, keys)
	if err != nil {
		return nil, err
	}

	var pageKeys []id.ID
	switch ec := entry.Contents.(*api.Document_Entry).Entry.Contents.(type) {
	case *api.Entry_PageKeys:
		pageKeys, err = api.GetEntryPageKeys(entry)
		if err != nil {
			return nil, err
		}
	case *api.Entry_Page:
		_, docKey, err := api.GetPageDocument(ec.Page)
		if err != nil {
			return nil, err
		}
		pageKeys = []id.ID{docKey}
	}
	return metadata, u.scanner.Scan(content, pageKeys, keys, metadata)
}

func newEntryDoc(
	authorPub []byte,
	pageIDs []id.ID,
	encMeta *enc.EncryptedMetadata,
	docL storage.DocumentLoader,
) (*api.Document, error) {

	var entry *api.Entry
	var err error
	if len(pageIDs) == 1 {
		entry, err = newSinglePageEntry(authorPub, pageIDs[0], encMeta, docL)
	} else {
		entry, err = newMultiPageEntry(authorPub, pageIDs, encMeta)
	}
	if err != nil {
		return nil, err
	}

	doc := &api.Document{
		Contents: &api.Document_Entry{
			Entry: entry,
		},
	}
	return doc, nil
}

func newSinglePageEntry(
	authorPub []byte,
	pageKey id.ID,
	encMeta *enc.EncryptedMetadata,
	docL storage.DocumentLoader,
) (*api.Entry, error) {

	pageDoc, err := docL.Load(pageKey)
	if err != nil {
		return nil, err
	}
	pageContent, ok := pageDoc.Contents.(*api.Document_Page)
	if !ok {
		return nil, errors.New("not a page")
	}
	return &api.Entry{
		AuthorPublicKey: authorPub,
		Contents: &api.Entry_Page{
			Page: pageContent.Page,
		},
		CreatedTime:           time.Now().Unix(),
		MetadataCiphertext:    encMeta.Ciphertext,
		MetadataCiphertextMac: encMeta.CiphertextMAC,
	}, nil
}

func newMultiPageEntry(
	authorPub []byte, pageKeys []id.ID, encMeta *enc.EncryptedMetadata,
) (*api.Entry, error) {

	pageKeyBytes := make([][]byte, len(pageKeys))
	for i, pageKey := range pageKeys {
		pageKeyBytes[i] = pageKey.Bytes()
	}

	return &api.Entry{
		AuthorPublicKey: authorPub,
		Contents: &api.Entry_PageKeys{
			PageKeys: &api.PageKeys{Keys: pageKeyBytes},
		},
		CreatedTime:           time.Now().Unix(),
		MetadataCiphertext:    encMeta.Ciphertext,
		MetadataCiphertextMac: encMeta.CiphertextMAC,
	}, nil
}
