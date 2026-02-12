package backend

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

type UsersListResult struct {
	Items      []*model.User
	NextCursor string
}

type Users struct {
	store storage.UserStore
}

func NewUsers(store storage.UserStore) *Users {
	return &Users{store: store}
}

func (x *Users) List(ctx context.Context, limit int, cursor string, filter storage.UserFilter) (*UsersListResult, error) {
	if limit <= 0 {
		limit = 5
	}

	res, err := x.store.ListUsers(ctx, filter, storage.ListOptions{
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return nil, err
	}

	return &UsersListResult{
		Items:      res.Items,
		NextCursor: res.NextCursor,
	}, nil
}

// --- Create / Update / Delete ---

type UsersCreateRequest struct {
	ID      string
	Subject string
	Email   string
	Name    string

	RoleIDs     []string
	Permissions []kind.Permission

	Disabled bool
}

func (x *Users) Create(ctx context.Context, id, subject, name, email string) (*model.User, error) {
	if id == "" || subject == "" {
		return nil, storage.ErrInvalidArgument
	}

	// Ensure ID is free.
	if _, err := x.store.GetUser(ctx, id); err == nil {
		return nil, storage.ErrAlreadyExists
	} else if err != nil && err != storage.ErrNotFound {
		return nil, err
	}

	// Ensure Subject is free.
	if _, err := x.store.GetUserBySubject(ctx, subject); err == nil {
		return nil, storage.ErrAlreadyExists
	} else if err != nil && err != storage.ErrNotFound {
		return nil, err
	}

	u, err := model.NewUser(id, subject)
	if err != nil {
		return nil, storage.ErrInvalidArgument
	}

	if name != "" {
		u.NameAdd(name)
	}
	if email != "" {
		u.EmailAdd(email)
	}

	if err := x.store.UpsertUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

type UsersUpdateRequest struct {
	ID string

	Email *string
	Name  *string

	// nil => don't touch roles/perms
	RoleIDs     *[]string
	Permissions *[]kind.Permission

	// nil => don't touch
	Disabled *bool
}

func (x *Users) Update(ctx context.Context, req UsersUpdateRequest) (*model.User, error) {
	u, err := x.store.GetUser(ctx, req.ID)
	if err != nil {
		return nil, err
	}

	if req.Email != nil {
		u.EmailAdd(*req.Email)
	}
	if req.Name != nil {
		u.NameAdd(*req.Name)
	}

	if req.Disabled != nil {
		if *req.Disabled {
			u.Disable()
		} else {
			u.Enable()
		}
	}

	if req.RoleIDs != nil {
		want := make(map[string]struct{}, len(*req.RoleIDs))
		for _, rid := range *req.RoleIDs {
			if rid == "" {
				// keep strict: let model/storage semantics surface as caller error
				return nil, storage.ErrInvalidArgument
			}
			want[rid] = struct{}{}
		}

		for _, cur := range u.RoleIDsAll() {
			if _, ok := want[cur]; !ok {
				u.RoleDelete(cur)
			}
		}
		for rid := range want {
			if err := u.RoleAdd(rid); err != nil {
				return nil, err
			}
		}
	}

	if req.Permissions != nil {
		want := make(map[kind.Permission]struct{}, len(*req.Permissions))
		for _, p := range *req.Permissions {
			if p == "" {
				return nil, storage.ErrInvalidArgument
			}
			want[p] = struct{}{}
		}

		for _, cur := range u.PermissionsAll() {
			if _, ok := want[cur]; !ok {
				u.PermissionDelete(cur)
			}
		}
		for p := range want {
			if err := u.PermissionAdd(p); err != nil {
				return nil, err
			}
		}
	}

	if err := x.store.UpsertUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (x *Users) Delete(ctx context.Context, id string) error {
	return x.store.DeleteUser(ctx, id)
}
