package api

import (
	"errors"
	"strconv"

	"prism-2api/internal/adminapi"
	"prism-2api/internal/clientkeys"
)

// keyGroupRenamer 适配 groups.KeyGroupRenamer（Store.RenameGroup 有返回值）。
type keyGroupRenamer struct{ *clientkeys.Store }

func (r keyGroupRenamer) RenameGroup(old, newName string) {
	if r.Store != nil {
		r.Store.RenameGroup(old, newName)
	}
}

type clientKeyFacade struct{ store *clientkeys.Store }

func newClientKeyFacade(store *clientkeys.Store) adminapi.ClientKeyStore {
	if store == nil {
		return nil
	}
	return clientKeyFacade{store}
}

func toAdminKey(k clientkeys.Key) adminapi.ClientKey {
	return adminapi.ClientKey{
		ID:          strconv.FormatUint(k.ID, 10),
		MaskedKey:   clientkeys.Mask(k.Key),
		Name:        k.Name,
		Description: k.Description,
		Disabled:    k.Disabled,
		Group:       k.Group,
		TotalCalls:  int64(k.TotalCalls),
		TotalTokens: int64(k.TotalInputTokens + k.TotalOutputTokens),
		CreatedAt:   k.CreatedAt,
		LastUsedAt:  k.LastUsedAt,
		IsSystem:    k.IsSystem,
	}
}

func parseClientKeyID(id string) (uint64, error) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return 0, errors.New("invalid client key id")
	}
	return n, nil
}

func (f clientKeyFacade) List() []adminapi.ClientKey {
	raw := f.store.List()
	out := make([]adminapi.ClientKey, 0, len(raw))
	for _, k := range raw {
		out = append(out, toAdminKey(k))
	}
	return out
}

func (f clientKeyFacade) Create(req adminapi.CreateClientKeyRequest) (*adminapi.CreateClientKeyResponse, error) {
	if req.Name == "" {
		return nil, errors.New("name is required")
	}
	k := f.store.Create(req.Name, req.Description, req.Group)
	return &adminapi.CreateClientKeyResponse{
		ID:        strconv.FormatUint(k.ID, 10),
		Key:       k.Key,
		Name:      k.Name,
		CreatedAt: k.CreatedAt,
	}, nil
}

func (f clientKeyFacade) Get(id string) (*adminapi.ClientKey, error) {
	n, err := parseClientKeyID(id)
	if err != nil {
		return nil, err
	}
	k, ok := f.store.Get(n)
	if !ok {
		return nil, errors.New("client key not found")
	}
	item := toAdminKey(k)
	return &item, nil
}

func (f clientKeyFacade) Update(id string, req adminapi.UpdateClientKeyRequest) (*adminapi.ClientKey, error) {
	n, err := parseClientKeyID(id)
	if err != nil {
		return nil, err
	}
	if _, ok := f.store.Get(n); !ok {
		return nil, errors.New("client key not found")
	}
	if req.Disabled != nil {
		f.store.SetDisabled(n, *req.Disabled)
	}
	f.store.UpdateMeta(n, req.Name, req.Description, req.Group)
	k, ok := f.store.Get(n)
	if !ok {
		return nil, errors.New("client key not found")
	}
	item := toAdminKey(k)
	return &item, nil
}

func (f clientKeyFacade) Delete(id string) error {
	n, err := parseClientKeyID(id)
	if err != nil {
		return err
	}
	if f.store.IsSystem(n) {
		return errors.New("system key cannot be deleted")
	}
	if !f.store.Delete(n) {
		return errors.New("client key not found")
	}
	return nil
}
