package eks

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/ec2"
	humanize "github.com/dustin/go-humanize"
	"go.uber.org/zap"
)

// SECURITY NOTE: MAKE SURE PRIVATE KEY NEVER GETS UPLOADED TO CLOUD STORAGE AND DLETE AFTER USE!!!

func (md *embedded) createKeyPair() (err error) {
	if md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName == "" {
		return errors.New("cannot create key pair without key name")
	}
	if md.cfg.WorkerNodePrivateKeyPath == "" {
		return errors.New("cannot create key pair without private key path")
	}

	now := time.Now().UTC()

	var output *ec2.CreateKeyPairOutput
	output, err = md.ec2.CreateKeyPair(&ec2.CreateKeyPairInput{
		KeyName: aws.String(md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName),
	})
	if err != nil {
		return err
	}
	md.cfg.ClusterState.StatusKeyPairCreated = true
	md.cfg.Sync()

	if *output.KeyName != md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName {
		return fmt.Errorf("unexpected key name %q, expected %q", *output.KeyName, md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName)
	}
	if err = os.MkdirAll(filepath.Dir(md.cfg.WorkerNodePrivateKeyPath), 0700); err != nil {
		return err
	}
	if err = ioutil.WriteFile(
		md.cfg.WorkerNodePrivateKeyPath,
		[]byte(*output.KeyMaterial),
		0400,
	); err != nil {
		return err
	}

	md.lg.Info(
		"created key pair",
		zap.String("key-name", md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName),
		zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
	)
	return md.cfg.Sync()
}

func (md *embedded) deleteKeyPair() error {
	if !md.cfg.ClusterState.StatusKeyPairCreated {
		return nil
	}
	defer func() {
		os.RemoveAll(md.cfg.WorkerNodePrivateKeyPath)
		md.cfg.ClusterState.StatusKeyPairCreated = false
		md.cfg.Sync()
	}()

	if md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName == "" {
		return errors.New("cannot delete key pair without key name")
	}

	now := time.Now().UTC()

	_, err := md.ec2.DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyName: aws.String(md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName),
	})
	if err != nil {
		return err
	}

	time.Sleep(time.Second)

	for i := 0; i < 10; i++ {
		_, err = md.ec2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
			KeyNames: aws.StringSlice([]string{md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName}),
		})
		if err == nil {
			break
		}

		if request.IsErrorRetryable(err) || request.IsErrorThrottle(err) {
			md.lg.Warn("failed to describe key pair, retrying...", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		// https://docs.aws.amazon.com/AWSEC2/latest/APIReference/errors-overview.html
		awsErr, ok := err.(awserr.Error)
		if ok && awsErr.Code() == "InvalidKeyPair.NotFound" {
			md.lg.Info(
				"deleted key pair from AWS resources",
				zap.String("key-name", md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName),
				zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
			)
			return nil
		}
		return err
	}
	return fmt.Errorf("deleted key pair but %q still exists", md.cfg.ClusterState.CFStackWorkerNodeGroupKeyPairName)
}

// isKeyPairDeletedAWSCLI returns true if error indicates that
// the key has already been deleted.
func isKeyPairDeletedAWSCLI(err error, keyName string) bool {
	if err == nil {
		return false
	}
	// An error occurred (InvalidKeyPair.NotFound) when calling the DescribeKeyPairs operation: The key pair 'leegyuho-invalid' does not exist
	return strings.Contains(err.Error(), fmt.Sprintf("An error occurred (InvalidKeyPair.NotFound) when calling the DescribeKeyPairs operation: The key pair '%s' does not exist", keyName))
}

// isKeyPairDeletedGoClient returns true if error indicates that
// the key has already been deleted.
func isKeyPairDeletedGoClient(err error, keyName string) bool {
	if err == nil {
		return false
	}
	// InvalidKeyPair.NotFound: The key pair 'aws-k8s-tester-20180915-TESTID-liROD5s-KEY-PAIR' does not exist
	return strings.Contains(err.Error(), fmt.Sprintf("InvalidKeyPair.NotFound: The key pair '%s' does not exist", keyName))
}
