package eks

import (
	"fmt"
	"os"

	"github.com/aws/aws-k8s-tester/eks"
	"github.com/aws/aws-k8s-tester/eksconfig"
	"github.com/aws/aws-k8s-tester/ekstester"
	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	"github.com/spf13/cobra"
)

func newS3Upload() *cobra.Command {
	return &cobra.Command{
		Use:   "s3-upload [local file path] [remote S3 path]",
		Short: "Upload a file to S3",
		Run:   s3UploadFunc,
	}
}

func s3UploadFunc(cmd *cobra.Command, args []string) {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "expected 2 arguments, got %v\n", args)
		os.Exit(1)
	}

	if !fileutil.Exist(path) {
		fmt.Fprintf(os.Stderr, "cannot find configuration %q\n", path)
		os.Exit(1)
	}

	cfg, err := eksconfig.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load configuration %q (%v)\n", path, err)
		os.Exit(1)
	}

	var tester ekstester.Tester
	tester, err = eks.NewTester(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create EKS deployer %v\n", err)
		os.Exit(1)
	}

	from, to := args[0], args[1]
	if err = tester.UploadToBucketForTests(from, to); err != nil {
		fmt.Fprintf(os.Stderr, "failed to upload from %q to %q (%v)\n", from, to, err)
		os.Exit(1)
	}

	fmt.Println("'aws-k8s-tester eks upload s3' success")
}
