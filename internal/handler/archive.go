package handler

import (
	"context"

	"github.com/redmemo/redmemo/internal/reddit"
)

func (h *Handler) fetchSubreddit(ctx context.Context, sub, sort, after string, limit int) ([]reddit.Post, string, string, error) {
	if h.oauthPool.HasAvailableTokens() {
		posts, before, after, err := h.redditCli.FetchSubreddit(ctx, sub, sort, after, limit)
		if err == nil {
			return posts, before, after, nil
		}
	}
	return h.publicCli.FetchSubreddit(ctx, sub, sort, after, limit)
}

func (h *Handler) fetchPost(ctx context.Context, sub, id, commentSort string) (reddit.Post, []reddit.Comment, error) {
	if h.oauthPool.HasAvailableTokens() {
		post, comments, err := h.redditCli.FetchPost(ctx, sub, id, commentSort)
		if err == nil {
			return post, comments, nil
		}
	}
	return h.publicCli.FetchPost(ctx, sub, id, commentSort)
}

func (h *Handler) fetchSubredditAbout(ctx context.Context, sub string) (reddit.Subreddit, error) {
	if h.oauthPool.HasAvailableTokens() {
		info, err := h.redditCli.FetchSubredditAbout(ctx, sub)
		if err == nil {
			return info, nil
		}
	}
	return h.publicCli.FetchSubredditAbout(ctx, sub)
}
