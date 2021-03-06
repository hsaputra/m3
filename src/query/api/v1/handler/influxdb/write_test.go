// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package influxdb

import (
	"fmt"
	"testing"

	imodels "github.com/influxdata/influxdb/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// human-readable string out of what the iterator produces;
// they are easiest for human to handle
func (self *ingestIterator) pop(t *testing.T) string {
	if self.Next() {
		tags, dp, _, _ := self.Current()
		assert.Equal(t, 1, len(dp))

		return fmt.Sprintf("%s %v %s", tags.String(), dp[0].Value, dp[0].Timestamp)
	}
	return ""
}

func TestIngestIterator(t *testing.T) {
	// test prometheus-illegal measure and label components (should be _s)
	// as well as all value types influxdb supports
	s := `?measure:!,?tag1:!=tval1,?tag2:!=tval2 ?key1:!=3,?key2:!=2i 1574838670386469800
?measure:!,?tag1:!=tval1,?tag2:!=tval2 ?key3:!="string",?key4:!=T 1574838670386469801
`
	points, err := imodels.ParsePoints([]byte(s))
	require.NoError(t, err)
	iter := &ingestIterator{points: points, promRewriter: newPromRewriter()}
	require.NoError(t, iter.Error())
	for _, line := range []string{
		"__name__: _measure:___key1:_, _tag1__: tval1, _tag2__: tval2 3 2019-11-27 07:11:10.3864698 +0000 UTC",
		"__name__: _measure:___key2:_, _tag1__: tval1, _tag2__: tval2 2 2019-11-27 07:11:10.3864698 +0000 UTC",
		"__name__: _measure:___key4:_, _tag1__: tval1, _tag2__: tval2 1 2019-11-27 07:11:10.386469801 +0000 UTC",
		"",
		"",
	} {
		assert.Equal(t, line, iter.pop(t))
	}
	require.NoError(t, iter.Error())
}

func TestIngestIteratorDuplicateTag(t *testing.T) {
	// Ensure that duplicate tag causes error and no metrics entries
	s := `measure,lab!=2,lab?=3 key=2i 1574838670386469800
`
	points, err := imodels.ParsePoints([]byte(s))
	require.NoError(t, err)
	iter := &ingestIterator{points: points, promRewriter: newPromRewriter()}
	require.NoError(t, iter.Error())
	for _, line := range []string{
		"",
	} {
		assert.Equal(t, line, iter.pop(t))
	}
	require.EqualError(t, iter.Error(), "non-unique Prometheus label lab_")
}

func TestIngestIteratorDuplicateNameTag(t *testing.T) {
	// Ensure that duplicate name tag causes error and no metrics entries
	s := `measure,__name__=x key=2i 1574838670386469800
`
	points, err := imodels.ParsePoints([]byte(s))
	require.NoError(t, err)
	iter := &ingestIterator{points: points, promRewriter: newPromRewriter()}
	require.NoError(t, iter.Error())
	for _, line := range []string{
		"",
	} {
		assert.Equal(t, line, iter.pop(t))
	}
	require.EqualError(t, iter.Error(), "non-unique Prometheus label __name__")
}
