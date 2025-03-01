// Copyright 2021-2022 the Kubeapps contributors.
// SPDX-License-Identifier: Apache-2.0
package v1alpha1

import (
	"context"
	"encoding/json"
	"fmt"

	packages "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/packages/v1alpha1"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/pkg/paginate"
)

const CompleteToken = -1

// summaryWithOffsets is the channel type for the results of the combined
// core results after fanning in from the plugins.
type summaryWithOffsets struct {
	availablePackageSummary *packages.AvailablePackageSummary
	categories              []string
	nextItemOffsets         map[string]int
	err                     error
}

// fanInAvailablePackageSummaries fans in the results from the separate plugins
// to the return channel.
//
// Each plugin handles the request in a separate go-routine while this function
// uses the fan-in pattern to merge those results, sending the next result back
// down the return channel until the request is satisfied. Importantly, each
// result is accompanied by the current next item offsets for each plugin so
// that the caller can generate a next page token that is able to encode the
// offsets for each plugin. The next request the begins each plugin where it
// left off for the last.
//
// Plugins generally do not use snapshots of the actual data, so, similar to the
// pagination of individual plugins, it will be possible that this returns
// duplicates or missing data if data is added or removed between paginated
// requests.
func fanInAvailablePackageSummaries(ctx context.Context, pkgPlugins []pkgPluginWithServer, request *packages.GetAvailablePackageSummariesRequest) (<-chan summaryWithOffsets, error) {
	summariesCh := make(chan summaryWithOffsets)

	corePageSize := int(request.GetPaginationOptions().GetPageSize())
	// We'll request a bit more than pageSize / n from each plugin. If the page
	// size is 10 and we have 3 plugins, request 5 items from each to start.
	pluginPageSize := corePageSize
	if len(pkgPlugins) > 1 {
		pluginPageSize = pluginPageSize / (len(pkgPlugins) - 1)
	}

	pluginPageOffsets := map[string]int{}
	if request.GetPaginationOptions().GetPageToken() != "" {
		err := json.Unmarshal([]byte(request.GetPaginationOptions().GetPageToken()), &pluginPageOffsets)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal %q: %w", request.GetPaginationOptions().GetPageToken(), err)
		}
	}

	fanInput := []<-chan *summaryWithOffset{}
	for _, pluginWithSrv := range pkgPlugins {
		// Importantly, each plugin needs its own request, with its own pagination
		// options.
		r := &packages.GetAvailablePackageSummariesRequest{
			Context:       request.Context,
			FilterOptions: request.FilterOptions,
			PaginationOptions: &packages.PaginationOptions{
				PageSize:  int32(pluginPageSize),
				PageToken: fmt.Sprintf("%d", pluginPageOffsets[pluginWithSrv.plugin.Name]),
			},
		}

		ch, err := sendAvailablePackageSummariesForPlugin(ctx, pluginWithSrv, r)
		if err != nil {
			return nil, err
		}
		fanInput = append(fanInput, ch)
	}

	// We now have a slice of channels for the fan-in and want a go routine that
	// will ensure it sends the next (ordered) item from all channels down the
	// channel.
	go func() {
		numSent := 0
		nextItems := make([]*summaryWithOffset, len(fanInput))
		for {
			// Populate the empty next items from each channel.
			for i, ch := range fanInput {
				if nextItems[i] == nil {
					// If the channel is closed, the value will remain nil.
					ok := true
					nextItems[i], ok = <-ch
					if !ok {
						// If the channel was closed, we reached the last item for that
						// plugin. We need to recognise when all plugins have exhausted
						// itemsoffsets
						pluginName := pkgPlugins[i].plugin.Name
						pluginPageOffsets[pluginName] = CompleteToken
					}

					if nextItems[i] != nil && nextItems[i].err != nil {
						summariesCh <- summaryWithOffsets{
							err: nextItems[i].err,
						}
						close(summariesCh)
						return
					}
				}
			}

			// Choose the minimum by name and send it down the line.
			// First find the first non-nil value as the min.
			minIndex := -1
			for i, s := range nextItems {
				if s != nil {
					minIndex = i
					break
				}
			}

			// If there is no non-nil value left, we're done.
			if minIndex == -1 {
				close(summariesCh)
				return
			}

			// Otherwise, we find the minimum item of the next items from each channel.
			for i, s := range nextItems {
				if s != nil && s.availablePackageSummary.Name < nextItems[minIndex].availablePackageSummary.Name {
					minIndex = i
				}
			}
			pluginName := nextItems[minIndex].availablePackageSummary.GetAvailablePackageRef().GetPlugin().GetName()
			pluginPageOffsets[pluginName] = nextItems[minIndex].nextItemOffset
			summariesCh <- summaryWithOffsets{
				availablePackageSummary: nextItems[minIndex].availablePackageSummary,
				categories:              nextItems[minIndex].categories,
				nextItemOffsets:         pluginPageOffsets,
			}
			// Ensure the item will get replaced on the next round.
			nextItems[minIndex] = nil

			numSent += 1
			if numSent == corePageSize {
				close(summariesCh)
				return
			}
		}
	}()

	return summariesCh, nil
}

// summaryWithOffset is the channel type for the single result from a
// single plugin.
type summaryWithOffset struct {
	availablePackageSummary *packages.AvailablePackageSummary
	categories              []string
	nextItemOffset          int
	err                     error
}

// sendAvailablePackageSummariesForPlugin returns a channel and sends the
// available package summaries returned by the plugin for the given request.
func sendAvailablePackageSummariesForPlugin(ctx context.Context, pkgPlugin pkgPluginWithServer, request *packages.GetAvailablePackageSummariesRequest) (<-chan *summaryWithOffset, error) {
	summaryCh := make(chan *summaryWithOffset)

	itemOffset, err := paginate.ItemOffsetFromPageToken(request.GetPaginationOptions().GetPageToken())
	if err != nil {
		return nil, err
	}

	if itemOffset == -1 {
		// This plugin was already exhausted during the last request. Nothing to do here.
		close(summaryCh)
		return summaryCh, nil
	}

	// Start a go func that requests the next page of summaries and sends them down the
	// channel. Since the channel is blocking, further requests won't be issued until the
	// previous response is drained. We could use a small buffer to request ahead as an
	// improvement.
	go func() {
		for {
			response, err := pkgPlugin.server.GetAvailablePackageSummaries(ctx, request)
			if err != nil {
				summaryCh <- &summaryWithOffset{err: err}
				close(summaryCh)
				return
			}
			categories := response.Categories
			for _, summary := range response.AvailablePackageSummaries {
				itemOffset = itemOffset + 1
				summaryCh <- &summaryWithOffset{
					availablePackageSummary: summary,
					categories:              categories,
					nextItemOffset:          itemOffset,
				}
				// We only need to send the categories once per response.
				categories = nil
			}
			if response.GetNextPageToken() == "" {
				close(summaryCh)
				return
			}
			// We can sanity check here to be sure the next page token
			// corresponds to the current value of itemOffset.
			if fmt.Sprintf("%d", itemOffset) != response.GetNextPageToken() {
				summaryCh <- &summaryWithOffset{
					err: fmt.Errorf("inconsistent item offset: got: %q, expected: %d", response.GetNextPageToken(), itemOffset),
				}
			}
			request.PaginationOptions.PageToken = response.GetNextPageToken()
		}
	}()

	return summaryCh, nil
}
