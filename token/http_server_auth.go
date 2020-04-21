package token

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/influxdata/httprouter"
	"github.com/influxdata/influxdb/v2"
	icontext "github.com/influxdata/influxdb/v2/context"
	kithttp "github.com/influxdata/influxdb/v2/kit/transport/http"
	"go.uber.org/zap"
)

type AuthHandler struct {
	chi.Router
	api           *kithttp.API
	log           *zap.Logger
	authSvc       influxdb.AuthorizationService
	lookupService influxdb.LookupService
	tenantService influxdb.TenantService
}

// NewHTTPAuthHandler constructs a new http server.
func NewHTTPAuthHandler(log *zap.Logger, authService influxdb.AuthorizationService) {
	h := &AuthHandler{
		api:     kithttp.NewAPI(kithttp.WithLog(log)),
		log:     log,
		authSvc: authService,
	}

	r := chi.NewRouter()
	r.Use(
		middleware.Recoverer,
		middleware.RequestID,
		middleware.RealIP,
	)

	r.Route("/", func(r chi.Router) {
		r.Post("/", h.handlePostAuthorization)
		r.Get("/", h.handleGetAuthorizations)

		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.handleGetAuthorization)
		})
	})
}

// handlePostAuthorization is the HTTP handler for the POST /api/v2/authorizations route.
func (h *AuthHandler) handlePostAuthorization(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := decodePostAuthorizationRequest(ctx, r)
	if err != nil {
		h.api.Err(w, err)
		return
	}

	user, err := getAuthorizedUser(r, h.tenantService)
	if err != nil {
		h.api.Err(w, influxdb.ErrUnableToCreateToken)
		return
	}

	userID := user.ID
	if req.UserID != nil && req.UserID.Valid() {
		userID = *req.UserID
	}

	auth := req.toinfluxdb()

	org, err := h.tenantService.FindOrganizationByID(ctx, auth.OrgID)
	if err != nil {
		h.api.Err(w, influxdb.ErrUnableToCreateToken)
		return
	}

	if err := h.authSvc.CreateAuthorization(ctx, auth); err != nil {
		h.api.Err(w, err)
		return
	}

	perms, err := newPermissionsResponse(ctx, auth.Permissions, h.lookupService)
	if err != nil {
		h.api.Err(w, err)
		return
	}

	h.log.Debug("Auth created ", zap.String("auth", fmt.Sprint(auth)))

	// if err := encodeResponse(ctx, w, http.StatusCreated, newAuthResponse(auth, org, user, perms)); err != nil {
	// 	logEncodingError(h.log, r, err)
	// 	return
	// }

	h.api.Respond(w, http.StatusCreated, newAuthResponse(auth, org, user, perms))
}

type postAuthorizationRequest struct {
	Status      influxdb.Status       `json:"status"`
	OrgID       influxdb.ID           `json:"orgID"`
	UserID      *influxdb.ID          `json:"userID,omitempty"`
	Description string                `json:"description"`
	Permissions []influxdb.Permission `json:"permissions"`
}

type authResponse struct {
	ID          influxdb.ID          `json:"id"`
	Token       string               `json:"token"`
	Status      influxdb.Status      `json:"status"`
	Description string               `json:"description"`
	OrgID       influxdb.ID          `json:"orgID"`
	Org         string               `json:"org"`
	UserID      influxdb.ID          `json:"userID"`
	User        string               `json:"user"`
	Permissions []permissionResponse `json:"permissions"`
	Links       map[string]string    `json:"links"`
	CreatedAt   time.Time            `json:"createdAt"`
	UpdatedAt   time.Time            `json:"updatedAt"`
}

func newAuthResponse(a *influxdb.Authorization, org *influxdb.Organization, user *influxdb.User, ps []permissionResponse) *authResponse {
	res := &authResponse{
		ID:          a.ID,
		Token:       a.Token,
		Status:      a.Status,
		Description: a.Description,
		OrgID:       a.OrgID,
		UserID:      a.UserID,
		User:        user.Name,
		Org:         org.Name,
		Permissions: ps,
		Links: map[string]string{
			"self": fmt.Sprintf("/api/v2/authorizations/%s", a.ID),
			"user": fmt.Sprintf("/api/v2/users/%s", a.UserID),
		},
		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
	}
	return res
}

// todo (al) this could probably just become a simple JSON (un)marshal ?
func (p *postAuthorizationRequest) toinfluxdb() *influxdb.Authorization {
	return &influxdb.Authorization{
		OrgID:       p.OrgID,
		Status:      p.Status,
		Description: p.Description,
		Permissions: p.Permissions,
		// UserID:      userID,
	}
}

type authsResponse struct {
	Links map[string]string `json:"links"`
	Auths []*authResponse   `json:"authorizations"`
}

func newAuthsResponse(as []*authResponse) *authsResponse {
	return &authsResponse{
		// TODO(desa): update links to include paging and filter information
		Links: map[string]string{
			"self": "/api/v2/authorizations",
		},
		Auths: as,
	}
}

func newPostAuthorizationRequest(a *influxdb.Authorization) (*postAuthorizationRequest, error) {
	res := &postAuthorizationRequest{
		OrgID:       a.OrgID,
		Description: a.Description,
		Permissions: a.Permissions,
		Status:      a.Status,
	}

	if a.UserID.Valid() {
		res.UserID = &a.UserID
	}

	res.SetDefaults()

	return res, res.Validate()
}

func (p *postAuthorizationRequest) SetDefaults() {
	if p.Status == "" {
		p.Status = influxdb.Active
	}
}

func (p *postAuthorizationRequest) Validate() error {
	if len(p.Permissions) == 0 {
		return &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "authorization must include permissions",
		}
	}

	for _, perm := range p.Permissions {
		if err := perm.Valid(); err != nil {
			return &influxdb.Error{
				Err: err,
			}
		}
	}

	if !p.OrgID.Valid() {
		return &influxdb.Error{
			Err:  influxdb.ErrInvalidID,
			Code: influxdb.EInvalid,
			Msg:  "org id required",
		}
	}

	if p.Status == "" {
		p.Status = influxdb.Active
	}

	err := p.Status.Valid()
	if err != nil {
		return err
	}

	return nil
}

type permissionResponse struct {
	Action   influxdb.Action  `json:"action"`
	Resource resourceResponse `json:"resource"`
}

type resourceResponse struct {
	influxdb.Resource
	Name         string `json:"name,omitempty"`
	Organization string `json:"org,omitempty"`
}

func newPermissionsResponse(ctx context.Context, ps []influxdb.Permission, svc influxdb.LookupService) ([]permissionResponse, error) {
	res := make([]permissionResponse, len(ps))
	for i, p := range ps {
		res[i] = permissionResponse{
			Action: p.Action,
			Resource: resourceResponse{
				Resource: p.Resource,
			},
		}

		if p.Resource.ID != nil {
			name, err := svc.Name(ctx, p.Resource.Type, *p.Resource.ID)
			if influxdb.ErrorCode(err) == influxdb.ENotFound {
				continue
			}
			if err != nil {
				return nil, err
			}
			res[i].Resource.Name = name
		}

		if p.Resource.OrgID != nil {
			name, err := svc.Name(ctx, influxdb.OrgsResourceType, *p.Resource.OrgID)
			if influxdb.ErrorCode(err) == influxdb.ENotFound {
				continue
			}
			if err != nil {
				return nil, err
			}
			res[i].Resource.Organization = name
		}
	}
	return res, nil
}

func decodePostAuthorizationRequest(ctx context.Context, r *http.Request) (*postAuthorizationRequest, error) {
	a := &postAuthorizationRequest{}
	if err := json.NewDecoder(r.Body).Decode(a); err != nil {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "invalid json structure",
			Err:  err,
		}
	}

	a.SetDefaults()

	return a, a.Validate()
}

func getAuthorizedUser(r *http.Request, svc influxdb.UserService) (*influxdb.User, error) {
	ctx := r.Context()

	a, err := icontext.GetAuthorizer(ctx)
	if err != nil {
		return nil, err
	}

	return svc.FindUserByID(ctx, a.GetUserID())
}

// handleGetAuthorizations is the HTTP handler for the GET /api/v2/authorizations route.
func (h *AuthHandler) handleGetAuthorizations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, err := decodeGetAuthorizationsRequest(ctx, r)
	if err != nil {
		h.log.Info("Failed to decode request", zap.String("handler", "getAuthorizations"), zap.Error(err))
		h.api.Err(w, err)
		return
	}

	opts := influxdb.FindOptions{}
	as, _, err := h.authSvc.FindAuthorizations(ctx, req.filter, opts)
	if err != nil {
		h.api.Err(w, err)
		return
	}

	f := req.filter
	// If the user or org name was provided, look up the ID first
	if f.User != nil {
		u, err := h.tenantService.FindUser(ctx, influxdb.UserFilter{Name: f.User})
		if err != nil {
			h.api.Err(w, err)
			return
		}
		f.UserID = &u.ID
	}

	if f.Org != nil {
		o, err := h.tenantService.FindOrganization(ctx, influxdb.OrganizationFilter{Name: f.Org})
		if err != nil {
			h.api.Err(w, err)
			return
		}
		f.OrgID = &o.ID
	}

	auths := make([]*authResponse, 0, len(as))
	for _, a := range as {
		o, err := h.tenantService.FindOrganizationByID(ctx, a.OrgID)
		if err != nil {
			h.log.Info("Failed to get organization", zap.String("handler", "getAuthorizations"), zap.String("orgID", a.OrgID.String()), zap.Error(err))
			continue
		}

		u, err := h.tenantService.FindUserByID(ctx, a.UserID)
		if err != nil {
			h.log.Info("Failed to get user", zap.String("handler", "getAuthorizations"), zap.String("userID", a.UserID.String()), zap.Error(err))
			continue
		}

		ps, err := newPermissionsResponse(ctx, a.Permissions, h.lookupService)
		if err != nil {
			h.api.Err(w, err)
			return
		}

		auths = append(auths, newAuthResponse(a, o, u, ps))
	}

	h.log.Debug("Auths retrieved ", zap.String("auths", fmt.Sprint(auths)))

	// if err := encodeResponse(ctx, w, http.StatusOK, newAuthsResponse(auths)); err != nil {
	// 	h.api.Err(w, err)
	// 	return
	// }

	h.api.Respond(w, http.StatusOK, newAuthsResponse(auths))
}

type getAuthorizationsRequest struct {
	filter influxdb.AuthorizationFilter
}

func decodeGetAuthorizationsRequest(ctx context.Context, r *http.Request) (*getAuthorizationsRequest, error) {
	qp := r.URL.Query()

	req := &getAuthorizationsRequest{}

	userID := qp.Get("userID")
	if userID != "" {
		id, err := influxdb.IDFromString(userID)
		if err != nil {
			return nil, err
		}
		req.filter.UserID = id
	}

	user := qp.Get("user")
	if user != "" {
		req.filter.User = &user
	}

	orgID := qp.Get("orgID")
	if orgID != "" {
		id, err := influxdb.IDFromString(orgID)
		if err != nil {
			return nil, err
		}
		req.filter.OrgID = id
	}

	org := qp.Get("org")
	if org != "" {
		req.filter.Org = &org
	}

	authID := qp.Get("id")
	if authID != "" {
		id, err := influxdb.IDFromString(authID)
		if err != nil {
			return nil, err
		}
		req.filter.ID = id
	}

	return req, nil
}

func (h *AuthHandler) handleGetAuthorization(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, err := decodeGetAuthorizationRequest(ctx, r)
	if err != nil {
		// TODO (al): logging middleware etc
		h.log.Info("Failed to decode request", zap.String("handler", "getAuthorization"), zap.Error(err))
		h.api.Err(w, err)
		return
	}

	a, err := h.authSvc.FindAuthorizationByID(ctx, req.ID)
	if err != nil {
		h.api.Err(w, err)
		return
	}

}

type getAuthorizationRequest struct {
	ID influxdb.ID
}

func decodeGetAuthorizationRequest(ctx context.Context, r *http.Request) (*getAuthorizationRequest, error) {
	params := httprouter.ParamsFromContext(ctx)
	id := params.ByName("id")
	if id == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing id",
		}
	}

	var i influxdb.ID
	if err := i.DecodeFromString(id); err != nil {
		return nil, err
	}

	return &getAuthorizationRequest{
		ID: i,
	}, nil
}
