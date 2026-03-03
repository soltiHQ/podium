# internal/bootstrap
Seed data is required for the control-plane to function on the first startup.

## Package map
```text
bootstrap/
└── bootstrap.go    Run() entry point, seedRoles, seedAdmin
```

## What gets seeded

### Roles
All roles defined in `domain/kind.BuiltinRoles` are upserted via `role.Service`.

### Admin user
A single admin user with a randomly generated password:
- ID: `user-admin`, subject: `admin`
- Role: `kind.RoleAdminID` (`001`)
- Password: crypto/rand, base64 URL-safe, 32 characters

The generated password is logged at **Warn** level on every startup
so the operator can retrieve it from the output.

## Startup flow
```text
cmd/main.go
    │
    ▼
bootstrap.Run(ctx, logger, roleSVC, userSVC, credSVC)
    │
    ├─ seedRoles     kind.BuiltinRoles → roleSVC.Upsert
    │
    └─ seedAdmin     model.NewUser → userSVC.Upsert
                     credentials.GeneratePassword → credSVC.SetPassword
                     logger.Warn (login + password)
```

## Dependencies
```text
bootstrap.Run
    ├── role.Service         upsert built-in roles
    ├── user.Service         upsert admin user
    └── credential.Service   set admin password
```
