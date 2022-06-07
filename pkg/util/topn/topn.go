/*
 * Copyright (c) 2017, MegaEase
 * All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package topn

import (
	"sort"
	"sync"

	"github.com/megaease/easegress/pkg/context"
	"github.com/megaease/easegress/pkg/util/httpstat"
	"github.com/megaease/easegress/pkg/util/urlclusteranalyzer"
)

type (
	// TopN is the statistics tool for HTTP traffic.
	TopN struct {
		m   sync.Map
		n   int
		uca *urlclusteranalyzer.URLClusterAnalyzer
	}

	// Item is the item of status.
	Item struct {
		Path string `yaml:"path"`
		*httpstat.Status
	}

	// Status contains all status generated by TopN.
	Status []*Item
)

func (s Status) Len() int           { return len(s) }
func (s Status) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s Status) Less(i, j int) bool { return s[i].Status.Count > s[j].Status.Count }

// New creates a TopN.
func New(n int) *TopN {
	return &TopN{
		n:   n,
		m:   sync.Map{},
		uca: urlclusteranalyzer.New(),
	}
}

// Stat stats the ctx.
func (t *TopN) Stat(ctx context.HTTPContext) {
	pattern := t.uca.GetPattern(ctx.Request().Path())

	var httpStat *httpstat.HTTPStat
	if v, loaded := t.m.Load(pattern); loaded {
		httpStat = v.(*httpstat.HTTPStat)
	} else {
		httpStat = httpstat.New()
		v, loaded = t.m.LoadOrStore(pattern, httpStat)
		if loaded {
			httpStat = v.(*httpstat.HTTPStat)
		}
	}

	httpStat.Stat(ctx.StatMetric())
}

// Status returns TopN Status, and resets all metrics.
func (t *TopN) Status() *Status {
	status := make(Status, 0)
	t.m.Range(func(key, value interface{}) bool {
		status = append(status, &Item{
			Path:   key.(string),
			Status: value.(*httpstat.HTTPStat).Status(),
		})
		return true
	})

	sort.Sort(status)
	n := len(status)
	if n > t.n {
		n = t.n
	}

	topNStatus := status[0:n]

	return &topNStatus
}