package archive

import "github.com/redmemo/redmemo/internal/reddit"

// IndexCommentsByID flattens a comment tree into an ID → Comment map, keeping
// only "t1" (real) nodes. Used by handlers to look up archived bodies for
// "more" stubs without re-asking Reddit.
func IndexCommentsByID(cs []reddit.Comment) map[string]reddit.Comment {
	out := map[string]reddit.Comment{}
	var walk func([]reddit.Comment)
	walk = func(arr []reddit.Comment) {
		for i := range arr {
			if arr[i].Kind == "t1" && arr[i].ID != "" {
				out[arr[i].ID] = arr[i]
			}
			walk(arr[i].Replies)
		}
	}
	walk(cs)
	return out
}

// ExpandMoreFromArchive walks `cs`; for any "more" stub whose listed child IDs
// are present in `archive`, inlines those archived comments in place of the
// stub (recursively expanding their own subtrees too). Child IDs absent from
// the archive stay behind in a trimmed-down "more" stub so the click-to-load
// affordance is preserved for the unseen ones.
func ExpandMoreFromArchive(cs []reddit.Comment, archive map[string]reddit.Comment) []reddit.Comment {
	if len(archive) == 0 {
		return cs
	}
	out := make([]reddit.Comment, 0, len(cs))
	for _, c := range cs {
		if c.Kind == "more" && c.ParentKind == "t1" && len(c.Children) > 0 {
			resolved := []reddit.Comment{}
			missing := []string{}
			for _, id := range c.Children {
				if arc, ok := archive[id]; ok {
					arc.Replies = ExpandMoreFromArchive(arc.Replies, archive)
					resolved = append(resolved, arc)
				} else {
					missing = append(missing, id)
				}
			}
			out = append(out, resolved...)
			if len(missing) > 0 {
				stub := c
				stub.Children = missing
				stub.MoreCount = int64(len(missing))
				out = append(out, stub)
			}
			continue
		}
		c.Replies = ExpandMoreFromArchive(c.Replies, archive)
		out = append(out, c)
	}
	return out
}

// MergeCommentTrees overlays `incoming` onto `prior`, returning a single tree
// that contains every t1 node from both (incoming wins on ID collisions). The
// shape preserves prior's root ordering: incoming-only roots are appended in
// arrival order. "more" stubs in either side whose children are now covered by
// real nodes are trimmed; fully-covered stubs are dropped entirely.
//
// This is what lets a partial /api/morechildren write extend the archive
// instead of clobbering it: without merge, each click would replace the
// full-tree snapshot with a 5-comment fragment, destroying the offline copy.
func MergeCommentTrees(prior, incoming []reddit.Comment) []reddit.Comment {
	// Index incoming t1 nodes — the freshest copy wins on collision.
	incomingByID := IndexCommentsByID(incoming)
	priorByID := IndexCommentsByID(prior)

	// Union of all known real-comment IDs (the "what we have" set used to
	// trim "more" stubs).
	known := make(map[string]bool, len(incomingByID)+len(priorByID))
	for id := range incomingByID {
		known[id] = true
	}
	for id := range priorByID {
		known[id] = true
	}

	// Walk prior, replacing collided nodes with the incoming copy and pruning
	// stubs whose children are now covered.
	merged := overlayTree(prior, incomingByID, known)

	// Append any incoming roots not already present in prior. Use prior's
	// flat index (NOT merged's) since merged was built from prior with IDs
	// unchanged.
	for _, c := range incoming {
		if c.Kind == "t1" && c.ID != "" {
			if _, ok := priorByID[c.ID]; ok {
				continue // already overlaid above
			}
			c.Replies = overlayTree(c.Replies, incomingByID, known)
			merged = append(merged, c)
			continue
		}
		// Trailing "more" stub from a fresh upstream fetch. Keep only if it
		// still references children we haven't seen yet.
		if c.Kind == "more" {
			trimmed := trimStub(c, known)
			if trimmed != nil {
				merged = append(merged, *trimmed)
			}
		}
	}
	return merged
}

// overlayTree walks a prior tree, replacing each node with its incoming copy
// when one exists, and trimming "more" stubs whose children are now covered by
// `known`. Replies are recursed.
func overlayTree(cs []reddit.Comment, incomingByID map[string]reddit.Comment, known map[string]bool) []reddit.Comment {
	out := make([]reddit.Comment, 0, len(cs))
	for _, c := range cs {
		if c.Kind == "more" {
			trimmed := trimStub(c, known)
			if trimmed != nil {
				out = append(out, *trimmed)
			}
			continue
		}
		if c.Kind != "t1" || c.ID == "" {
			out = append(out, c)
			continue
		}
		if fresh, ok := incomingByID[c.ID]; ok {
			// Use the fresh copy but merge replies recursively so prior's
			// expanded subtree isn't lost when the fresh copy's replies are
			// shorter (common when incoming came from /api/morechildren and
			// only knows about the requested subtree).
			fresh.Replies = mergeReplies(c.Replies, fresh.Replies, incomingByID, known)
			out = append(out, fresh)
			continue
		}
		c.Replies = overlayTree(c.Replies, incomingByID, known)
		out = append(out, c)
	}
	return out
}

// mergeReplies overlays incoming-side replies onto prior-side replies for the
// same parent. Mirrors MergeCommentTrees but scoped to one node's children.
func mergeReplies(prior, incoming []reddit.Comment, incomingByID map[string]reddit.Comment, known map[string]bool) []reddit.Comment {
	priorByID := map[string]bool{}
	for _, c := range prior {
		if c.Kind == "t1" && c.ID != "" {
			priorByID[c.ID] = true
		}
	}
	out := overlayTree(prior, incomingByID, known)
	for _, c := range incoming {
		if c.Kind == "t1" && c.ID != "" {
			if priorByID[c.ID] {
				continue
			}
			c.Replies = overlayTree(c.Replies, incomingByID, known)
			out = append(out, c)
			continue
		}
		if c.Kind == "more" {
			trimmed := trimStub(c, known)
			if trimmed != nil {
				// Avoid duplicate stubs: drop if prior already has an
				// equivalent one (same parent, same children set after
				// trimming). Simple shape check suffices.
				dup := false
				for _, p := range out {
					if p.Kind == "more" && p.ParentID == trimmed.ParentID {
						dup = true
						break
					}
				}
				if !dup {
					out = append(out, *trimmed)
				}
			}
		}
	}
	return out
}

// trimStub returns a copy of `stub` whose Children list excludes any ID we
// already have a real archived copy of. Returns nil when nothing is left to
// load (the stub is fully resolved and should be dropped).
func trimStub(stub reddit.Comment, known map[string]bool) *reddit.Comment {
	remaining := make([]string, 0, len(stub.Children))
	for _, id := range stub.Children {
		if !known[id] {
			remaining = append(remaining, id)
		}
	}
	if len(remaining) == 0 && len(stub.Children) > 0 {
		return nil
	}
	out := stub
	out.Children = remaining
	if int64(len(remaining)) < out.MoreCount {
		out.MoreCount = int64(len(remaining))
	}
	return &out
}
