package backend

import (
	"context"

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

func (u *Users) List(ctx context.Context, limit int, cursor string) (*UsersListResult, error) {
	if limit <= 0 {
		limit = 5
	}

	res, err := u.store.ListUsers(
		ctx,
		nil,
		storage.ListOptions{
			Limit:  limit,
			Cursor: cursor,
		},
	)
	if err != nil {
		return nil, err
	}

	return &UsersListResult{
		Items:      res.Items,
		NextCursor: res.NextCursor,
	}, nil
}
