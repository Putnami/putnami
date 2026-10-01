package iam

import (
	"time"

	"go.putnami.dev/database"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
)

// UsersPath is the public route that lists the feature's users.
const UsersPath = "/users"

// maxListedUsers bounds one listing.
const maxListedUsers = 100

// User is one identity the iam feature owns, as GET /users lists it.
type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Region      string    `json:"region"`
	CreatedAt   time.Time `json:"createdAt"`
}

// ListUsers answers GET /users with the most recent users, read from the
// iam_users table this feature's own migrations create. It is the sample's
// business read: a 200 proves the workload reached a database its migrations
// were applied to, which a health endpoint alone does not.
//
// It takes the *database.Pools registry rather than the *database.Pool: the
// registry opens the pool on first use, so describing the application at build
// time needs no database, while the unnamed pool is opened when the handler is
// wired.
func ListUsers(pools *database.Pools, ctx *http.Context) *http.Response {
	pool, err := pools.Default()
	if err != nil {
		logger.FromContextOrDefault(ctx.Context()).ErrorCtx(ctx.Context(), "open the users datasource", err)
		return http.InternalError("Internal Server Error")
	}
	rows, err := pool.Query(ctx.Context(),
		`SELECT id, email, display_name, region, created_at FROM iam_users ORDER BY created_at DESC, id LIMIT $1`,
		maxListedUsers)
	if err != nil {
		logger.FromContextOrDefault(ctx.Context()).ErrorCtx(ctx.Context(), "list users", err)
		return http.InternalError("Internal Server Error")
	}
	defer rows.Close()

	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Email, &user.DisplayName, &user.Region, &user.CreatedAt); err != nil {
			logger.FromContextOrDefault(ctx.Context()).ErrorCtx(ctx.Context(), "scan user", err)
			return http.InternalError("Internal Server Error")
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		logger.FromContextOrDefault(ctx.Context()).ErrorCtx(ctx.Context(), "list users", err)
		return http.InternalError("Internal Server Error")
	}
	return http.JSON(users)
}
