package sqlstore

import (
	"context"
	"fmt"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// A role's row holds its name beside the body, for the lookup: indexed, not
// unique, as in the in-memory store.

var (
	roleKind   = kind[*model.Role]{"podium_roles", snapshot.UnmarshalRoleJSON}
	upsertRole = upsertSQL("podium_roles", "created_at", "name", "body")
)

// UpsertRole creates or replaces a role.
func (w *writer) UpsertRole(ctx context.Context, r *model.Role) error {
	if err := storage.ValidateEntity(r); err != nil {
		return err
	}
	body, err := snapshot.MarshalRoleJSON(r)
	if err != nil {
		return err
	}
	id, err := indexKey("role ID", r.ID())
	if err != nil {
		return err
	}
	name, err := indexKey("role's name", r.Name())
	if err != nil {
		return err
	}
	created, err := columnTime("creation time", r.CreatedAt())
	if err != nil {
		return err
	}
	_, err = w.exec(ctx, upsertRole, id, created, name, body)
	return err
}

// GetRole returns the role with the ID.
func (r *reader) GetRole(ctx context.Context, id string) (*model.Role, error) {
	return byID(ctx, r, roleKind, id)
}

// GetRoles returns one role per ID asked for, in the order asked, repeats
// included, each a copy of its own; storage.ErrNotFound when an ID has no role.
func (r *reader) GetRoles(ctx context.Context, ids []string) ([]*model.Role, error) {
	if len(ids) == 0 {
		return nil, storage.ErrInvalidArgument
	}
	var (
		unique []any
		seen   = make(map[string]bool, len(ids))
	)
	for _, id := range ids {
		if id == "" {
			return nil, storage.ErrInvalidArgument
		}
		if !seen[id] {
			seen[id] = true
			unique = append(unique, bin(id))
		}
	}
	found := make(map[string]*model.Role, len(unique))
	// In chunks, so that no statement holds more placeholders than Postgres takes.
	for len(unique) > 0 {
		chunk := unique[:min(len(unique), roleChunk)]
		unique = unique[len(chunk):]
		if err := r.rolesByID(ctx, chunk, found); err != nil {
			return nil, err
		}
	}
	out := make([]*model.Role, len(ids))
	for i, id := range ids {
		role, ok := found[id]
		if !ok {
			return nil, storage.ErrNotFound
		}
		out[i] = role.Clone()
	}
	return out, nil
}

// roleChunk is the most IDs one statement of GetRoles looks for.
const roleChunk = 1000

// rolesByID reads the roles whose IDs are ids, bound as bytea, into found by ID.
func (r *reader) rolesByID(ctx context.Context, ids []any, found map[string]*model.Role) error {
	rows, err := r.q.QueryContext(ctx, r.d.Rebind("SELECT id, body FROM "+roleKind.table+" WHERE id IN ("+marks(len(ids))+")"), ids...)
	if err != nil {
		return r.fail(ctx, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, body []byte
		if err := rows.Scan(&id, &body); err != nil {
			return r.fail(ctx, err)
		}
		role, err := roleKind.decode(body)
		if err != nil {
			return err
		}
		found[string(id)] = role
	}
	if err := rows.Err(); err != nil {
		return r.fail(ctx, err)
	}
	return nil
}

// GetRoleByName returns the role with the name: exactly, by bytes. Two roles of
// one name are a fault, storage.ErrInternal.
func (r *reader) GetRoleByName(ctx context.Context, name string) (*model.Role, error) {
	if name == "" {
		return nil, storage.ErrInvalidArgument
	}
	return lookup(ctx, r, roleKind, "name = ?", bin(name))
}

// ListRoles returns a page of the roles. The store builds no role filter, so a
// filter other than nil is one this store did not build:
// storage.ErrInvalidArgument.
func (r *reader) ListRoles(ctx context.Context, filter storage.RoleFilter, opts storage.ListOptions) (*storage.RoleListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filter != nil {
		return nil, fmt.Errorf("%w: sqlstore: the store builds no role filter", storage.ErrInvalidArgument)
	}
	return listPage(ctx, r, pageQuery[*model.Role]{kind: roleKind, where: &where{}}, opts)
}

// DeleteRole deletes the role with the ID.
func (w *writer) DeleteRole(ctx context.Context, id string) error {
	return w.deleteByID(ctx, roleKind.table, id)
}

// UpsertRole creates or replaces a role, in a write transaction of its own.
func (s *Store) UpsertRole(ctx context.Context, r *model.Role) error {
	return s.write(ctx, func(w *writer) error { return w.UpsertRole(ctx, r) })
}

// ListRoles returns a page of the roles, from the replica for a read-only
// context.
func (s *Store) ListRoles(ctx context.Context, filter storage.RoleFilter, opts storage.ListOptions) (*storage.RoleListResult, error) {
	return routed(ctx, s, func(r *reader) (*storage.RoleListResult, error) { return r.ListRoles(ctx, filter, opts) })
}

// DeleteRole deletes the role with the ID, in a write transaction of its own.
func (s *Store) DeleteRole(ctx context.Context, id string) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteRole(ctx, id) })
}
