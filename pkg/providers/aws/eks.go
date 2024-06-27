package aws

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/pkg/errors"
	"github.com/projectdiscovery/cloudlist/pkg/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
)

// eksProvider is a provider for AWS EKS API.
type eksProvider struct {
	id        string
	eksClient *eks.EKS
	session   *session.Session
	regions   *ec2.DescribeRegionsOutput
}

func (ep *eksProvider) name() string {
	return "eks"
}

// GetResource returns all the resources in the store for a provider.
func (ep *eksProvider) GetResource(ctx context.Context) (*schema.Resources, error) {
	list := schema.NewResources()
	for _, region := range ep.regions.Regions {
		regionName := *region.RegionName
		ep.eksClient = eks.New(ep.session, aws.NewConfig().WithRegion(regionName))
		if resources, err := ep.listEKSResources(ep.eksClient); err == nil {
			list.Merge(resources)
		}
	}
	return list, nil
}

func (ep *eksProvider) listEKSResources(eksClient *eks.EKS) (*schema.Resources, error) {
	list := schema.NewResources()
	req := &eks.ListClustersInput{
		MaxResults: aws.Int64(100),
	}
	for {
		clustersOutput, err := eksClient.ListClusters(req)
		if err != nil {
			return nil, errors.Wrap(err, "could not list EKS clusters")
		}
		// Iterate over each cluster
		for _, clusterName := range clustersOutput.Clusters {
			// describe cluster
			clusterOutput, err := eksClient.DescribeCluster(&eks.DescribeClusterInput{
				Name: clusterName,
			})
			if err != nil {
				return nil, errors.Wrapf(err, "could not describe EKS cluster: %s", *clusterName)
			}
			clientset, err := newClientset(clusterOutput.Cluster)
			if err != nil {
				return nil, errors.Wrapf(err, "could not create clientset for EKS cluster: %s", *clusterName)
			}
			nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return nil, errors.Wrapf(err, "could not list nodes for EKS cluster: %s", *clusterName)
			}
			// Iterate over each node
			for _, node := range nodes.Items {
				var podIPs []string
				// List IP addresses of pods running on the node
				pods, err := clientset.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{
					FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.GetName()),
				})
				if err != nil {
					continue
				}
				// Collect pod IP addresses
				for _, pod := range pods.Items {
					for _, podIP := range pod.Status.PodIPs {
						podIPs = append(podIPs, podIP.IP)
					}
				}
				// Node IP
				nodeIP := node.Status.Addresses[0].Address
				list.Append(&schema.Resource{
					Provider:   providerName,
					ID:         node.GetName(),
					PublicIPv4: nodeIP,
					Public:     true,
					Service:   ep.name(),
				})
				// Pod IPs
				for _, podIP := range podIPs {
					list.Append(&schema.Resource{
						Provider:    providerName,
						ID:          node.GetName(),
						PrivateIpv4: podIP,
						Public:      false,
						Service:     ep.name(),
					})
				}
			}
		}
		if aws.StringValue(clustersOutput.NextToken) == "" {
			break
		}
		req.SetNextToken(*clustersOutput.NextToken)
	}
	return list, nil
}

func newClientset(cluster *eks.Cluster) (*kubernetes.Clientset, error) {
	gen, err := token.NewGenerator(true, false)
	if err != nil {
		return nil, err
	}
	opts := &token.GetTokenOptions{
		ClusterID: aws.StringValue(cluster.Name),
	}
	tok, err := gen.GetWithOptions(opts)
	if err != nil {
		return nil, err
	}
	ca, err := base64.StdEncoding.DecodeString(aws.StringValue(cluster.CertificateAuthority.Data))
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(
		&rest.Config{
			Host:        aws.StringValue(cluster.Endpoint),
			BearerToken: tok.Token,
			TLSClientConfig: rest.TLSClientConfig{
				CAData: ca,
			},
		},
	)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}
