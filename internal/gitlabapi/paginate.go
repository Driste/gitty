package gitlabapi

import (
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

const perPage = 100

// paginate walks every page of a GitLab list endpoint and concatenates the
// results. The fetch closure receives the gitlab.ListOptions for one page
// and returns that page's items, the response (used for NextPage), and any
// error. PerPage is fixed at 100 here so all callers share the same page
// size (FR-006).
func paginate[T any](
	fetch func(opts gitlab.ListOptions) ([]T, *gitlab.Response, error),
) ([]T, error) {
	var all []T
	var page int64 = 1
	for {
		items, resp, err := fetch(gitlab.ListOptions{PerPage: perPage, Page: page})
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return all, nil
}
