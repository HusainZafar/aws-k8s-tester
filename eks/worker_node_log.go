package eks

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aws/aws-k8s-tester/ec2config"
	"github.com/aws/aws-k8s-tester/internal/ssh"
	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	"go.uber.org/zap"
)

func (md *embedded) GetWorkerNodeLogs() (err error) {
	if !md.cfg.EnableWorkerNodeSSH {
		return errors.New("node SSH is not enabled")
	}

	var fpathToS3Path map[string]string
	fpathToS3Path, err = fetchWorkerNodeLogs(
		md.lg,
		"ec2-user", // for Amazon Linux 2
		md.cfg.ClusterName,
		md.cfg.WorkerNodePrivateKeyPath,
		md.cfg.ClusterState.WorkerNodes,
	)

	md.ec2InstancesLogMu.Lock()
	md.cfg.ClusterState.WorkerNodeLogs = fpathToS3Path
	md.ec2InstancesLogMu.Unlock()

	md.cfg.Sync()
	md.lg.Info("updated worker node logs", zap.String("synced-config-path", md.cfg.ConfigPath))

	return nil
}

type fetchResponse struct {
	data map[string]string
	err  error
}

func fetchWorkerNodeLog(
	lg *zap.Logger,
	userName string,
	clusterName string,
	privateKeyPath string,
	workerNode ec2config.Instance) (fpathToS3Path map[string]string, err error) {
	fpathToS3Path = make(map[string]string)

	id, ip := workerNode.InstanceID, workerNode.PublicIP
	pfx := strings.TrimSpace(fmt.Sprintf("%s-%s", id, ip))

	var sh ssh.SSH
	sh, err = ssh.New(ssh.Config{
		Logger:        lg,
		KeyPath:       privateKeyPath,
		PublicIP:      ip,
		PublicDNSName: workerNode.PublicDNSName,
		UserName:      userName,
	})
	if err != nil {
		lg.Warn(
			"failed to create SSH",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.Error(err),
		)
		return nil, err
	}

	if err = sh.Connect(); err != nil {
		lg.Warn(
			"failed to connect",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.Error(err),
		)
		return nil, err
	}

	var out []byte
	var fpath string
	var cmd string

	// https://github.com/awslabs/amazon-eks-ami/blob/master/files/logrotate-kube-proxy
	cmd = "cat /var/log/kube-proxy.log"
	lg.Debug(
		"fetching kube-proxy logs",
		zap.String("instance-id", id),
		zap.String("public-ip", ip),
		zap.String("cmd", cmd),
	)
	out, err = sh.Run(cmd, ssh.WithVerbose(false))
	if err != nil {
		sh.Close()
		lg.Warn(
			"failed to run command",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.String("cmd", cmd),
			zap.Error(err),
		)
		return nil, err
	}
	fpath, err = fileutil.WriteToTempDir(pfx+".kube-proxy.log", out)
	if err != nil {
		sh.Close()
		lg.Warn(
			"failed to write output",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.Error(err),
		)
		return nil, err
	}
	fpathToS3Path[fpath] = filepath.Join(clusterName, pfx, filepath.Base(fpath))

	// kernel logs
	lg.Info(
		"fetching kernel logs",
		zap.String("instance-id", id),
		zap.String("public-ip", ip),
	)
	cmd = "sudo journalctl --no-pager --output=short-precise -k"
	out, err = sh.Run(cmd, ssh.WithVerbose(false))
	if err != nil {
		sh.Close()
		lg.Warn(
			"failed to run command",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.String("cmd", cmd),
			zap.Error(err),
		)
		return nil, err
	}
	fpath, err = fileutil.WriteToTempDir(pfx+".kernel.log", out)
	if err != nil {
		sh.Close()
		lg.Warn(
			"failed to write output",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.Error(err),
		)
		return nil, err
	}
	fpathToS3Path[fpath] = filepath.Join(clusterName, pfx, filepath.Base(fpath))

	// full journal logs (e.g. disk mounts)
	lg.Debug(
		"fetching journal logs",
		zap.String("instance-id", id),
		zap.String("public-ip", ip),
	)
	cmd = "sudo journalctl --no-pager --output=short-precise"
	out, err = sh.Run(cmd, ssh.WithVerbose(false))
	if err != nil {
		sh.Close()
		lg.Warn(
			"failed to run command",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.String("cmd", cmd),
			zap.Error(err),
		)
		return nil, err
	}
	fpath, err = fileutil.WriteToTempDir(pfx+".journal.log", out)
	if err != nil {
		sh.Close()
		lg.Warn(
			"failed to write output",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.Error(err),
		)
		return nil, err
	}
	fpathToS3Path[fpath] = filepath.Join(clusterName, pfx, filepath.Base(fpath))

	// other systemd services
	cmd = "sudo systemctl list-units -t service --no-pager --no-legend --all"
	lg.Debug(
		"fetching all systemd services",
		zap.String("instance-id", id),
		zap.String("public-ip", ip),
		zap.String("cmd", cmd),
	)
	out, err = sh.Run(cmd, ssh.WithVerbose(false))
	if err != nil {
		sh.Close()
		lg.Warn(
			"failed to run command",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.String("cmd", cmd),
			zap.Error(err),
		)
		return nil, err
	}

	/*
		auditd.service                                        loaded    active   running Security Auditing Service
		auth-rpcgss-module.service                            loaded    inactive dead    Kernel Module supporting RPCSEC_GSS
	*/
	var svcs []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "" || len(fields) < 5 {
			continue
		}
		if fields[1] == "not-found" {
			continue
		}
		if fields[2] == "inactive" {
			continue
		}
		svcs = append(svcs, fields[0])
	}
	for _, svc := range svcs {
		cmd = "sudo journalctl --no-pager --output=cat -u " + svc
		lg.Debug(
			"fetching systemd service log",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.String("service", svc),
			zap.String("cmd", cmd),
		)
		out, err = sh.Run(cmd, ssh.WithVerbose(false))
		if err != nil {
			lg.Warn(
				"failed to run command",
				zap.String("instance-id", id),
				zap.String("public-ip", ip),
				zap.String("cmd", cmd),
				zap.Error(err),
			)
			continue
		}
		if len(out) == 0 {
			lg.Info("empty log", zap.String("service", svc))
			continue
		}
		fpath, err = fileutil.WriteToTempDir(pfx+"."+svc+".log", out)
		if err != nil {
			sh.Close()
			lg.Warn(
				"failed to write output",
				zap.String("instance-id", id),
				zap.String("public-ip", ip),
				zap.Error(err),
			)
			return nil, err
		}
		fpathToS3Path[fpath] = filepath.Join(clusterName, pfx, filepath.Base(fpath))
	}

	// other /var/log
	cmd = "sudo find /var/log ! -type d"
	lg.Debug(
		"fetching all /var/log",
		zap.String("instance-id", id),
		zap.String("public-ip", ip),
		zap.String("cmd", cmd),
	)
	out, err = sh.Run(cmd, ssh.WithVerbose(false))
	if err != nil {
		sh.Close()
		lg.Warn(
			"failed to run command",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.String("cmd", cmd),
			zap.Error(err),
		)
		return nil, err
	}
	var varLogs []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) == 0 {
			// last value
			continue
		}
		varLogs = append(varLogs, line)
	}
	for _, p := range varLogs {
		cmd = "sudo cat " + p
		lg.Debug(
			"fetching /var/log",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.String("cmd", cmd),
		)
		out, err = sh.Run(cmd, ssh.WithVerbose(false))
		if err != nil {
			lg.Warn(
				"failed to run command",
				zap.String("instance-id", id),
				zap.String("public-ip", ip),
				zap.String("cmd", cmd),
				zap.Error(err),
			)
			continue
		}
		if len(out) == 0 {
			lg.Info("empty log", zap.String("path", p))
			continue
		}
		fpath, err = fileutil.WriteToTempDir(pfx+strings.Replace(p, "/", ".", -1), out)
		if err != nil {
			sh.Close()
			lg.Warn(
				"failed to write output",
				zap.String("instance-id", id),
				zap.String("public-ip", ip),
				zap.Error(err),
			)
			return nil, err
		}
		fpathToS3Path[fpath] = filepath.Join(clusterName, pfx, filepath.Base(fpath))
	}

	sh.Close()

	return fpathToS3Path, nil
}

func fetchWorkerNodeLogs(
	lg *zap.Logger,
	userName string,
	clusterName string,
	privateKeyPath string,
	workerNodes map[string]ec2config.Instance) (fpathToS3Path map[string]string, err error) {

	// create channel
	c := make(chan fetchResponse)
	const batchN = 200

	// create new map fpathToS3Path to join all the data and slice to hold any errors
	fpathToS3Path = make(map[string]string)
	possibleErrors := make(map[string]int)

	// loop through nodes and send goroutine
	i := 0
	for _, iv := range workerNodes {
		go concurrentFetchLog(lg, userName, clusterName, privateKeyPath, iv, c)
		i++
		// batch and join data from batch, then reset counter.
		if i == batchN {
			joinData(c, fpathToS3Path, i, possibleErrors)
			i = 0
		}
	}
	remainder := len(workerNodes) % batchN
	if remainder > 0 {
		joinData(c, fpathToS3Path, remainder, possibleErrors)
	}

	// once joinData is complete, join possibleErrors into one
	if len(possibleErrors) > 0 {
		var sb strings.Builder
		for strErr, occ := range possibleErrors {
			str := fmt.Sprintf("%v: %v, ", strErr, occ)
			sb.WriteString(str)
		}
		err = errors.New(sb.String())
	}

	// return map of all data collected
	return fpathToS3Path, err
}

func concurrentFetchLog(
	lg *zap.Logger,
	userName string,
	clusterName string,
	privateKeyPath string,
	workerNode ec2config.Instance,
	ch chan fetchResponse) {

	// send request to fetchWorkerLog
	fm, e := fetchWorkerNodeLog(lg, userName, clusterName, privateKeyPath, workerNode)

	// send map received to channel
	ch <- fetchResponse{data: fm, err: e}
}

func joinData(
	channel chan fetchResponse,
	joinedData map[string]string,
	desired int,
	errCollection map[string]int) {

	for i := 0; i < desired; i++ {
		resp := <-channel
		dataSubset := resp.data
		for k, v := range dataSubset {
			joinedData[k] = v
		}
		if resp.err != nil {
			errCollection[resp.err.Error()]++
		}
	}
}
