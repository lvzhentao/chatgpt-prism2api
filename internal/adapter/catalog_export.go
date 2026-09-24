package adapter

// NewStaticCatalog returns a TTL catalog seeded with a static model list.
// Site.NewCatalog should call this and optionally SetClientProvider so Load()
// can refresh from the vendor API.
func NewStaticCatalog(models []*ModelInfo) Catalog {
	return newMemoryCatalog(models)
}
