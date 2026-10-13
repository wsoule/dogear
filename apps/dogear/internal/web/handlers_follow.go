package web

import (
	"net/http"
	"net/url"

	"dogear/internal/service"
)

// followBox drives the "followbox" partial on profiles: follower and
// following counts, plus the follow toggle for other signed-in viewers.
type followBox struct {
	Username             string
	Followers, Following int
	On                   bool // the viewer follows Username
	CanFollow            bool // signed in and not the owner
	CSRF                 string
}

// profileFollowBox builds the follow box from a loaded profile.
func profileFollowBox(p service.Profile, v *service.Actor, csrf string) followBox {
	return followBox{Username: p.User.Username, Followers: p.FollowerCount, Following: p.FollowingCount,
		On: p.ViewerFollows, CanFollow: v != nil && !p.IsOwner && !p.Blocked, CSRF: csrf}
}

// follow is POST /u/{username}/follow: follow=1 follows, anything else
// unfollows. htmx gets the refreshed follow box; plain forms go back to the
// profile.
func (s *Server) follow(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	v := viewer(r)
	var err error
	if r.FormValue("follow") == "1" {
		err = s.svc.Follow(r.Context(), v, username)
	} else {
		err = s.svc.Unfollow(r.Context(), v, username)
	}
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/u/"+url.PathEscape(username), http.StatusSeeOther)
		return
	}
	u, err := s.svc.GetUserByUsername(r.Context(), username)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	followers, following, err := s.svc.FollowCounts(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, http.StatusOK, "following", "followbox", followBox{Username: u.Username, Followers: followers,
		Following: following, On: s.svc.IsFollowing(r.Context(), v, u.ID), CanFollow: true, CSRF: reqCtx(r).csrf})
}

// followingPage is /following: recent public saves by people the viewer
// follows, plus suggestions when there is nothing to show yet.
type followingPage struct {
	View      service.FollowingView
	Suggested []service.SuggestedPerson
	CSRF      string
	Viewer    *service.Actor
	// Next is the htmx URL for the next page of rows, NextPage the no-JS link.
	Next, NextPage string
}

// suggestedPeopleCount is how many people the empty /following suggests.
const suggestedPeopleCount = 12

// following serves /following, or only the next page of rows for htmx.
func (s *Server) following(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	if v == nil {
		s.needSignIn(w, r)
		return
	}
	cursor := r.URL.Query().Get("before")
	fv, err := s.svc.FollowingFeed(r.Context(), v, cursor)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	fp := followingPage{View: fv, CSRF: reqCtx(r).csrf, Viewer: v}
	if fv.Next != "" {
		fp.NextPage = "/following?before=" + url.QueryEscape(fv.Next)
		fp.Next = fp.NextPage
	}
	if cursor != "" && r.Header.Get("HX-Request") != "" {
		s.fragment(w, http.StatusOK, "following", "followingrows", fp)
		return
	}
	if len(fv.Saves) == 0 && cursor == "" {
		if fp.Suggested, err = s.svc.SuggestedPeople(r.Context(), v, suggestedPeopleCount); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "following", &Page{Title: "following", Nav: "following", NoIndex: true, Data: fp})
}

// recentStripSize is how many "recently saved" lines the home page shows.
const recentStripSize = 6
