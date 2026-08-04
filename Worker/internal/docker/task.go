package docker

import "context"

// ListTasks lists tasks, optionally filtered. Pass nil to list all.
func (c *httpClient) ListTasks(ctx context.Context, f Filter) ([]Task, error) {
	q := filtersQuery(f)
	var tasks []Task
	if err := c.getJSON(ctx, "/tasks", q, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// ServiceTasks returns all tasks belonging to a service.
func (c *httpClient) ServiceTasks(ctx context.Context, serviceID string) ([]Task, error) {
	return c.ListTasks(ctx, Filter{"service": {serviceID}})
}
