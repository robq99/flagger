package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"

	flaggerv1 "github.com/weaveworks/flagger/pkg/apis/flagger/v1beta1"
)

const (
	cloudWatchMaxRetries                           = 3
	cloudWatchStartDeltaMultiplierOnMetricInterval = 10
)

type CloudWatchProvider struct {
	client     cloudWatchClient
	startDelta time.Duration
}

// for the testing purpose
type cloudWatchClient interface {
	GetMetricData(input *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error)
}

// NewCloudWatchProvider takes a metricInterval, a provider spec and the credentials map, and
// returns a cloudWatchProvider ready to execute queries against the AWS CloudWatch metrics
func NewCloudWatchProvider(metricInterval string, provider flaggerv1.MetricTemplateProvider) (*CloudWatchProvider, error) {
	region := strings.TrimLeft(provider.Address, "monitoring.")
	region = strings.TrimRight(region, ".amazonaws.com")
	sess, err := session.NewSession(
		aws.NewConfig().
			WithRegion(region).
			WithMaxRetries(cloudWatchMaxRetries).
			WithEndpoint(provider.Address),
	)

	md, err := time.ParseDuration(metricInterval)
	if err != nil {
		return nil, fmt.Errorf("error parsing metric interval: %s", err.Error())
	}

	return &CloudWatchProvider{
		client:     cloudwatch.New(sess),
		startDelta: cloudWatchStartDeltaMultiplierOnMetricInterval * md,
	}, err
}

// RunQuery executes the aws cloud watch metrics query against GetMetricsData endpoint
// and returns the the first result as float64
func (p *CloudWatchProvider) RunQuery(query string) (float64, error) {
	var cq []*cloudwatch.MetricDataQuery
	if err := json.Unmarshal([]byte(query), &cq); err != nil {
		return 0, fmt.Errorf("error unmarshaling query: %s", err.Error())
	}

	end := time.Now()
	start := end.Add(-p.startDelta)
	res, err := p.client.GetMetricData(&cloudwatch.GetMetricDataInput{
		EndTime:           aws.Time(end),
		MaxDatapoints:     aws.Int64(20),
		StartTime:         aws.Time(start),
		MetricDataQueries: cq,
	})

	if err != nil {
		return 0, fmt.Errorf("error requesting cloudwatch: %s", err.Error())
	}

	mr := res.MetricDataResults
	if len(mr) < 1 {
		return 0, fmt.Errorf("no values found in response: %s", res.String())
	}

	vs := res.MetricDataResults[0].Values
	if len(vs) < 1 {
		return 0, fmt.Errorf("no values found in response: %s", res.String())
	}

	return aws.Float64Value(vs[0]), nil
}

// IsOnline calls GetMetricsData endpoint with the empty query
// and returns an error if the returned status code is NOT http.StatusBadRequests.
// For example, if the flagger does not have permission to perform `cloudwatch:GetMetricsData`,
// the returned status code would be http.StatusForbidden
func (p *CloudWatchProvider) IsOnline() (bool, error) {
	_, err := p.client.GetMetricData(&cloudwatch.GetMetricDataInput{
		EndTime:           aws.Time(time.Time{}),
		MetricDataQueries: []*cloudwatch.MetricDataQuery{},
		StartTime:         aws.Time(time.Time{}),
	})

	if err == nil {
		return true, nil
	}

	ae, ok := err.(awserr.RequestFailure)
	if !ok {
		return false, fmt.Errorf("unexpected error: %v", err)
	} else if ae.StatusCode() != http.StatusBadRequest {
		return false, fmt.Errorf("unexpected status code: %v", ae)
	}
	return true, nil
}
