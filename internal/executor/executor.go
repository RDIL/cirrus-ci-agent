package executor

import (
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/avast/retry-go"
	"github.com/certifi/gocertifi"
	"github.com/cirruslabs/cirrus-ci-agent/api"
	"github.com/cirruslabs/cirrus-ci-agent/internal/cirrusenv"
	"github.com/cirruslabs/cirrus-ci-agent/internal/client"
	"github.com/cirruslabs/cirrus-ci-agent/internal/environment"
	"github.com/cirruslabs/cirrus-ci-agent/internal/executor/metrics"
	"github.com/cirruslabs/cirrus-ci-agent/internal/executor/terminalwrapper"
	"github.com/cirruslabs/cirrus-ci-agent/internal/executor/updatebatcher"
	"github.com/cirruslabs/cirrus-ci-agent/internal/executor/vaultunboxer"
	"github.com/cirruslabs/cirrus-ci-agent/internal/http_cache"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"golang.org/x/net/context"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type CommandAndLogs struct {
	Name string
	Cmd  *exec.Cmd
	Logs *LogUploader
}

type Executor struct {
	taskIdentification   *api.TaskIdentification
	serverToken          string
	backgroundCommands   []CommandAndLogs
	httpCacheHost        string
	commandFrom          string
	commandTo            string
	preCreatedWorkingDir string
	cacheAttempts        *CacheAttempts
	env                  *environment.Environment
	terminalWrapper      *terminalwrapper.Wrapper
}

type StepResult struct {
	Success        bool
	SignaledToExit bool
	Duration       time.Duration
}

var (
	ErrStepExit = errors.New("executor step requested to terminate execution")
)

func NewExecutor(
	taskId int64,
	clientToken,
	serverToken string,
	commandFrom string,
	commandTo string,
	preCreatedWorkingDir string,
) *Executor {
	taskIdentification := &api.TaskIdentification{
		TaskId: taskId,
		Secret: clientToken,
	}
	return &Executor{
		taskIdentification:   taskIdentification,
		serverToken:          serverToken,
		backgroundCommands:   make([]CommandAndLogs, 0),
		httpCacheHost:        "",
		commandFrom:          commandFrom,
		commandTo:            commandTo,
		preCreatedWorkingDir: preCreatedWorkingDir,
		cacheAttempts:        NewCacheAttempts(),
		env:                  environment.NewEmpty(),
	}
}

func (executor *Executor) RunBuild(ctx context.Context) {
	// Start collecting metrics
	metricsCtx, metricsCancel := context.WithCancel(ctx)
	defer metricsCancel()
	metricsResultChan := metrics.Run(metricsCtx, nil)

	log.Println("Getting initial commands...")

	var response *api.CommandsResponse
	var err error
	var numRetries uint

	err = retry.Do(
		func() error {
			response, err = client.CirrusClient.InitialCommands(ctx, &api.InitialCommandsRequest{
				TaskIdentification:  executor.taskIdentification,
				LocalTimestamp:      time.Now().Unix(),
				ContinueFromCommand: executor.commandFrom,
				Retry:               numRetries != 0,
			})
			return err
		}, retry.OnRetry(func(n uint, err error) {
			numRetries++
			log.Printf("Failed to get initial commands: %v", err)
		}),
		retry.Delay(5*time.Second),
		retry.Attempts(math.MaxUint32), retry.LastErrorOnly(true),
		retry.Context(ctx),
	)
	if err != nil {
		// Context was cancelled before we had a chance to get initial commands
		return
	}

	if response.ServerToken != executor.serverToken {
		log.Panic("Server token is incorrect!")
		return
	}

	executor.env.Merge(getScriptEnvironment(executor, response.Environment), false)

	// Unbox VAULT[...] environment variables
	var vaultUnboxer *vaultunboxer.VaultUnboxer

	for key, value := range executor.env.Items() {
		boxedValue, err := vaultunboxer.NewBoxedValue(value)
		if err != nil {
			if errors.Is(err, vaultunboxer.ErrNotABoxedValue) {
				continue
			}

			message := fmt.Sprintf("failed to parse a Vault-boxed value %s: %v", value, err)
			log.Println(message)
			executor.reportError(message)

			return
		}

		if vaultUnboxer == nil {
			vaultUnboxer, err = vaultunboxer.NewFromEnvironment(ctx, executor.env)
			if err != nil {
				message := fmt.Sprintf("failed to initialize a Vault client: %v", err)
				log.Println(message)
				executor.reportError(message)

				return
			}
		}

		unboxedValue, err := vaultUnboxer.Unbox(ctx, boxedValue)
		if err != nil {
			message := fmt.Sprintf("failed to unbox a Vault-boxed value %s: %v", value, err)
			log.Println(message)
			executor.reportError(message)

			return
		}

		executor.env.Set(key, unboxedValue)
		executor.env.AddSensitiveValues(unboxedValue)
	}

	workingDir, ok := executor.env.Lookup("CIRRUS_WORKING_DIR")
	if ok {
		EnsureFolderExists(workingDir)
		if err := os.Chdir(workingDir); err != nil {
			log.Printf("Failed to change current working directory to '%s': %v", workingDir, err)
		}
	} else {
		log.Printf("Not changing current working directory because CIRRUS_WORKING_DIR is not set")
	}

	commands := response.Commands

	if cacheHost, ok := os.LookupEnv("CIRRUS_HTTP_CACHE_HOST"); ok {
		executor.env.Set("CIRRUS_HTTP_CACHE_HOST", cacheHost)
	}

	if _, ok := executor.env.Lookup("CIRRUS_HTTP_CACHE_HOST"); !ok {
		executor.env.Set("CIRRUS_HTTP_CACHE_HOST", http_cache.Start(executor.taskIdentification))
	}

	executor.httpCacheHost = executor.env.Get("CIRRUS_HTTP_CACHE_HOST")
	subCtx, cancel := context.WithTimeout(ctx, time.Duration(response.TimeoutInSeconds)*time.Second)
	defer cancel()
	executor.env.AddSensitiveValues(response.SecretsToMask...)

	if len(commands) == 0 {
		return
	}

	// Launch terminal session for remote access (in case requested by the user)
	var hasWaitForTerminalInstruction bool
	var terminalServerAddress string

	for _, command := range commands {
		if instruction, ok := command.Instruction.(*api.Command_WaitForTerminalInstruction); ok {
			hasWaitForTerminalInstruction = true
			if instruction.WaitForTerminalInstruction != nil {
				terminalServerAddress = instruction.WaitForTerminalInstruction.TerminalServerAddress
			}
			break
		}
	}

	if hasWaitForTerminalInstruction {
		expireIn := 15 * time.Minute

		expireInString, ok := executor.env.Lookup("CIRRUS_TERMINAL_EXPIRATION_WINDOW")
		if ok {
			expireInInt, err := strconv.Atoi(expireInString)
			if err == nil {
				expireIn = time.Duration(expireInInt) * time.Second
			}
		}

		shellEnv := append(os.Environ(), EnvMapAsSlice(executor.env.Items())...)

		executor.terminalWrapper = terminalwrapper.New(subCtx, executor.taskIdentification, terminalServerAddress,
			expireIn, shellEnv)
	}

	failedAtLeastOnce := response.FailedAtLeastOnce

	ub := updatebatcher.New()

	for _, command := range BoundedCommands(commands, executor.commandFrom, executor.commandTo) {
		shouldRun := (command.ExecutionBehaviour == api.Command_ON_SUCCESS && !failedAtLeastOnce) ||
			(command.ExecutionBehaviour == api.Command_ON_FAILURE && failedAtLeastOnce) ||
			command.ExecutionBehaviour == api.Command_ALWAYS
		if !shouldRun {
			ub.Queue(&api.CommandResult{
				Name:   command.Name,
				Status: api.Status_SKIPPED,
			})
			continue
		}

		ub.Queue(&api.CommandResult{
			Name:   command.Name,
			Status: api.Status_EXECUTING,
		})
		ub.Flush(ctx, executor.taskIdentification)

		log.Printf("Executing %s...", command.Name)

		stepResult, err := executor.performStep(subCtx, command)
		if err != nil {
			return
		}

		if !stepResult.Success {
			failedAtLeastOnce = true
		}

		log.Printf("%s finished!", command.Name)

		var currentCommandStatus api.Status
		if stepResult.Success {
			currentCommandStatus = api.Status_COMPLETED
		} else {
			currentCommandStatus = api.Status_FAILED
		}
		ub.Queue(&api.CommandResult{
			Name:            command.Name,
			Status:          currentCommandStatus,
			DurationInNanos: stepResult.Duration.Nanoseconds(),
			SignaledToExit:  stepResult.SignaledToExit,
		})
	}

	ub.Flush(ctx, executor.taskIdentification)

	log.Printf("Background commands to clean up after: %d!\n", len(executor.backgroundCommands))
	for i := 0; i < len(executor.backgroundCommands); i++ {
		backgroundCommand := executor.backgroundCommands[i]
		log.Printf("Cleaning up after background command %s...\n", backgroundCommand.Name)
		err := backgroundCommand.Cmd.Process.Kill()
		if err != nil {
			backgroundCommand.Logs.Write([]byte(fmt.Sprintf("\nFailed to stop background script %s: %s!", backgroundCommand.Name, err)))
		}
		backgroundCommand.Logs.Finalize()
	}

	// Retrieve resource utilization metrics
	metricsCancel()

	var resourceUtilization *api.ResourceUtilization

	select {
	case metricsResult := <-metricsResultChan:
		for _, err := range metricsResult.Errors() {
			message := fmt.Sprintf("Encountered an error while gathering resource utilization metrics: %v", err)
			log.Print(message)
			_, _ = client.CirrusClient.ReportAgentWarning(ctx, &api.ReportAgentProblemRequest{
				TaskIdentification: executor.taskIdentification,
				Message:            message,
			})
		}
		resourceUtilization = metricsResult.ResourceUtilization
	case <-time.After(3 * time.Second):
		// Yes, we already use context.Context, but it seems that gopsutil is somewhat lacking it's support[1],
		// so we err on the side of caution here.
		//
		// [1]: https://github.com/shirou/gopsutil/issues/724
		message := "Failed to retrieve resource utilization metrics in time"
		log.Print(message)
		_, _ = client.CirrusClient.ReportAgentWarning(ctx, &api.ReportAgentProblemRequest{
			TaskIdentification: executor.taskIdentification,
			Message:            message,
		})
	}

	_ = retry.Do(
		func() error {
			_, err = client.CirrusClient.ReportAgentFinished(ctx, &api.ReportAgentFinishedRequest{
				TaskIdentification:     executor.taskIdentification,
				CacheRetrievalAttempts: executor.cacheAttempts.ToProto(),
				ResourceUtilization:    resourceUtilization,
				CommandResults:         ub.History(),
			})
			return err
		}, retry.OnRetry(func(n uint, err error) {
			log.Printf("Failed to report that the agent has finished: %v\nRetrying...\n", err)
		}),
		retry.Delay(10*time.Second),
		retry.Attempts(2),
		retry.Context(ctx),
	)
}

// BoundedCommands bounds a slice of commands with unique names to a half-open range [fromName, toName).
func BoundedCommands(commands []*api.Command, fromName, toName string) []*api.Command {
	left, right := 0, len(commands)

	for i, command := range commands {
		if fromName != "" && command.Name == fromName {
			left = i
		}

		if toName != "" && command.Name == toName {
			right = i
		}
	}

	return commands[left:right]
}

func getScriptEnvironment(executor *Executor, responseEnvironment map[string]string) map[string]string {
	if responseEnvironment == nil {
		responseEnvironment = make(map[string]string)
	}

	if _, ok := responseEnvironment["OS"]; !ok {
		if _, ok := os.LookupEnv("OS"); !ok {
			responseEnvironment["OS"] = runtime.GOOS
		}
	}
	responseEnvironment["CIRRUS_OS"] = runtime.GOOS
	responseEnvironment["CIRRUS_ARCH"] = runtime.GOARCH

	// Use directory created by the persistent worker if CIRRUS_WORKING_DIR
	// was not overridden in the task specification by the user
	_, hasWorkingDir := responseEnvironment["CIRRUS_WORKING_DIR"]
	if !hasWorkingDir && executor.preCreatedWorkingDir != "" {
		responseEnvironment["CIRRUS_WORKING_DIR"] = executor.preCreatedWorkingDir
	}

	if _, ok := responseEnvironment["CIRRUS_WORKING_DIR"]; !ok {
		defaultTempDirPath := filepath.Join(os.TempDir(), "cirrus-ci-build")
		if _, err := os.Stat(defaultTempDirPath); os.IsNotExist(err) {
			responseEnvironment["CIRRUS_WORKING_DIR"] = filepath.ToSlash(defaultTempDirPath)
		} else if executor.commandFrom != "" {
			// Default folder exists and we continue execution. Therefore we need to use it.
			responseEnvironment["CIRRUS_WORKING_DIR"] = filepath.ToSlash(defaultTempDirPath)
		} else {
			uniqueTempDirPath, _ := os.MkdirTemp(os.TempDir(), fmt.Sprintf("cirrus-task-%d", executor.taskIdentification.TaskId))
			responseEnvironment["CIRRUS_WORKING_DIR"] = filepath.ToSlash(uniqueTempDirPath)
		}
	}

	return responseEnvironment
}

func (executor *Executor) performStep(ctx context.Context, currentStep *api.Command) (*StepResult, error) {
	success := false
	signaledToExit := false
	start := time.Now()

	logUploader, err := NewLogUploader(ctx, executor, currentStep.Name)
	if err != nil {
		message := fmt.Sprintf("Failed to initialize command %s log upload: %v", currentStep.Name, err)

		_, _ = client.CirrusClient.ReportAgentWarning(ctx, &api.ReportAgentProblemRequest{
			TaskIdentification: executor.taskIdentification,
			Message:            message,
		})

		return &StepResult{
			Success:  false,
			Duration: time.Since(start),
		}, nil
	}

	if _, ok := currentStep.Instruction.(*api.Command_BackgroundScriptInstruction); !ok {
		defer logUploader.Finalize()
	}

	cirrusEnv, err := cirrusenv.New(executor.taskIdentification.TaskId)
	if err != nil {
		message := fmt.Sprintf("Failed initialize CIRRUS_ENV subsystem: %v", err)
		log.Print(message)
		fmt.Fprintln(logUploader, message)
		return &StepResult{
			Success:  false,
			Duration: time.Since(start),
		}, nil
	}
	defer cirrusEnv.Close()
	executor.env.Set("CIRRUS_ENV", cirrusEnv.Path())

	switch instruction := currentStep.Instruction.(type) {
	case *api.Command_ExitInstruction:
		return nil, ErrStepExit
	case *api.Command_CloneInstruction:
		success = executor.CloneRepository(ctx, logUploader, executor.env)
	case *api.Command_FileInstruction:
		success = executor.CreateFile(ctx, logUploader, instruction.FileInstruction, executor.env)
	case *api.Command_ScriptInstruction:
		cmd, err := executor.ExecuteScriptsStreamLogsAndWait(ctx, logUploader, currentStep.Name,
			instruction.ScriptInstruction.Scripts, executor.env)
		success = err == nil && cmd.ProcessState.Success()
		if err == nil {
			if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
				signaledToExit = ws.Signaled()
			}
		}
		if err == TimeOutError {
			signaledToExit = false
		}
	case *api.Command_BackgroundScriptInstruction:
		cmd, err := executor.ExecuteScriptsAndStreamLogs(ctx, logUploader,
			instruction.BackgroundScriptInstruction.Scripts, executor.env)
		if err == nil {
			executor.backgroundCommands = append(executor.backgroundCommands, CommandAndLogs{
				Name: currentStep.Name,
				Cmd:  cmd,
				Logs: logUploader,
			})
			log.Printf("Started execution of #%d background command %s\n", len(executor.backgroundCommands), currentStep.Name)
			success = true
		} else {
			log.Printf("Failed to create command line for background command %s: %s\n", currentStep.Name, err)
			_, _ = logUploader.Write([]byte(fmt.Sprintf("Failed to create command line: %s", err)))
			logUploader.Finalize()
			success = false
		}
	case *api.Command_CacheInstruction:
		success = executor.DownloadCache(ctx, logUploader, currentStep.Name, executor.httpCacheHost,
			instruction.CacheInstruction, executor.env)
	case *api.Command_UploadCacheInstruction:
		success = executor.UploadCache(ctx, logUploader, currentStep.Name, executor.httpCacheHost,
			instruction.UploadCacheInstruction)
	case *api.Command_ArtifactsInstruction:
		success = executor.UploadArtifacts(ctx, logUploader, currentStep.Name,
			instruction.ArtifactsInstruction, executor.env)
	case *api.Command_WaitForTerminalInstruction:
		operationChan := executor.terminalWrapper.Wait()

	WaitForTerminalInstructionFor:
		for {
			switch operation := (<-operationChan).(type) {
			case *terminalwrapper.LogOperation:
				log.Println(operation.Message)
				_, _ = fmt.Fprintln(logUploader, operation.Message)
			case *terminalwrapper.ExitOperation:
				success = operation.Success
				break WaitForTerminalInstructionFor
			}
		}
	default:
		log.Printf("Unsupported instruction %T", instruction)
		success = false
	}

	cirrusEnvVariables, err := cirrusEnv.Consume()
	if err != nil {
		message := fmt.Sprintf("Failed collect CIRRUS_ENV subsystem results: %v", err)
		log.Print(message)
		fmt.Fprintln(logUploader, message)
	}

	// Pick up new CIRRUS_ENV variables
	_, isSensitive := executor.env.Lookup("CIRRUS_ENV_SENSITIVE")
	executor.env.Merge(cirrusEnvVariables, isSensitive)

	return &StepResult{
		Success:        success,
		SignaledToExit: signaledToExit,
		Duration:       time.Since(start),
	}, nil
}

func (executor *Executor) ExecuteScriptsStreamLogsAndWait(
	ctx context.Context,
	logUploader *LogUploader,
	commandName string,
	scripts []string,
	env *environment.Environment) (*exec.Cmd, error) {
	cmd, err := ShellCommandsAndWait(ctx, scripts, env, func(bytes []byte) (int, error) {
		return logUploader.Write(bytes)
	}, executor.shouldKillProcesses())
	return cmd, err
}

func (executor *Executor) ExecuteScriptsAndStreamLogs(
	ctx context.Context,
	logUploader *LogUploader,
	scripts []string,
	env *environment.Environment,
) (*exec.Cmd, error) {
	sc, err := NewShellCommands(ctx, scripts, env, func(bytes []byte) (int, error) {
		return logUploader.Write(bytes)
	})
	var cmd *exec.Cmd
	if sc != nil {
		cmd = sc.cmd
	}
	return cmd, err
}

func (executor *Executor) CreateFile(
	ctx context.Context,
	logUploader *LogUploader,
	instruction *api.FileInstruction,
	env *environment.Environment,
) bool {
	switch source := instruction.GetSource().(type) {
	case *api.FileInstruction_FromEnvironmentVariable:
		envName := source.FromEnvironmentVariable
		content, is_provided := env.Lookup(envName)
		if !is_provided {
			logUploader.Write([]byte(fmt.Sprintf("Environment variable %s is not set! Skipping file creation...", envName)))
			return true
		}
		if strings.HasPrefix(content, "ENCRYPTED") {
			logUploader.Write([]byte(fmt.Sprintf("Environment variable %s wasn't decrypted! Skipping file creation...", envName)))
			return true
		}
		filePath := env.ExpandText(instruction.DestinationPath)
		EnsureFolderExists(filepath.Dir(filePath))
		err := os.WriteFile(filePath, []byte(content), 0644)
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("Failed to write file %s: %s!", filePath, err)))
			return false
		}
		logUploader.Write([]byte(fmt.Sprintf("Created file %s!", filePath)))
		return true
	default:
		log.Printf("Unsupported source %T", source)
		return false
	}
}

func (executor *Executor) CloneRepository(
	ctx context.Context,
	logUploader *LogUploader,
	env *environment.Environment,
) bool {
	logUploader.Write([]byte("Using built-in Git...\n"))

	working_dir := env.Get("CIRRUS_WORKING_DIR")
	change := env.Get("CIRRUS_CHANGE_IN_REPO")
	branch := env.Get("CIRRUS_BRANCH")
	pr_number, is_pr := env.Lookup("CIRRUS_PR")
	tag, is_tag := env.Lookup("CIRRUS_TAG")
	is_clone_modules := env.Get("CIRRUS_CLONE_SUBMODULES") == "true"

	clone_url := env.Get("CIRRUS_REPO_CLONE_URL")
	if _, has_clone_token := env.Lookup("CIRRUS_REPO_CLONE_TOKEN"); has_clone_token {
		clone_url = env.ExpandText("https://x-access-token:${CIRRUS_REPO_CLONE_TOKEN}@${CIRRUS_REPO_CLONE_HOST}/${CIRRUS_REPO_FULL_NAME}.git")
	}

	clone_depth := 0
	if depth_str, ok := env.Lookup("CIRRUS_CLONE_DEPTH"); ok {
		clone_depth, _ = strconv.Atoi(depth_str)
	}
	if clone_depth > 0 {
		logUploader.Write([]byte(fmt.Sprintf("\nLimiting clone depth to %d!", clone_depth)))
	}

	// if an environment doesn't have git installed most likely it an alpine container
	// which also most likely doesn't have CA certificates so SSL will fail :-(
	// let's configure CA certs our self!
	cert_pool, err := gocertifi.CACerts()
	if err != nil {
		logUploader.Write([]byte(fmt.Sprintf("\nFailed to get CA certificates: %s!", err)))
		return false
	}
	customClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: cert_pool},
		},
		Timeout: 900 * time.Second,
	}
	gitclient.InstallProtocol("https", githttp.NewClient(customClient))
	gitclient.InstallProtocol("http", githttp.NewClient(customClient))

	var repo *git.Repository

	if is_pr {
		repo, err = git.PlainInit(working_dir, false)
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to init repository: %s!", err)))
			return false
		}
		remoteConfig := &config.RemoteConfig{
			Name: "origin",
			URLs: []string{clone_url},
		}
		if _, err := repo.CreateRemote(remoteConfig); err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to create remote: %s!", err)))
			return false
		}

		headRefSpec := fmt.Sprintf("+refs/pull/%s/head:refs/remotes/origin/pull/%[1]s", pr_number)
		logUploader.Write([]byte(fmt.Sprintf("\nFetching %s...\n", headRefSpec)))
		fetchOptions := &git.FetchOptions{
			RemoteName: remoteConfig.Name,
			RefSpecs:   []config.RefSpec{config.RefSpec(headRefSpec)},
			Tags:       git.NoTags,
			Progress:   logUploader,
			Depth:      clone_depth,
		}
		err = repo.FetchContext(ctx, fetchOptions)
		if err != nil && strings.Contains(err.Error(), "couldn't find remote ref") {
			logUploader.Write([]byte("\nFailed to fetch head ref! Trying to fall back to merge ref..."))
			mergeRefSpec := fmt.Sprintf("+refs/pull/%s/merge:refs/remotes/origin/pull/%[1]s", pr_number)
			fetchOptions.RefSpecs = []config.RefSpec{config.RefSpec(mergeRefSpec)}
			if clone_depth > 0 {
				// increase by one since we are cloning with an extra "merge" commit from GH
				fetchOptions.Depth = clone_depth + 1
			}
			err = repo.FetchContext(ctx, fetchOptions)
		}
		if err != nil && retryableCloneError(err) {
			logUploader.Write([]byte(fmt.Sprintf("\nFetch failed: %s!", err)))
			logUploader.Write([]byte("\nRe-trying to fetch..."))
			err = repo.FetchContext(ctx, fetchOptions)
		}
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed fetch: %s!", err)))
			return false
		}

		workTree, err := repo.Worktree()
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to get work tree: %s!", err)))
			return false
		}

		checkoutOptions := git.CheckoutOptions{
			Hash: plumbing.NewHash(change),
		}
		logUploader.Write([]byte(fmt.Sprintf("\nChecking out %s...", checkoutOptions.Hash)))
		err = workTree.Checkout(&checkoutOptions)
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to checkout %s: %s!", checkoutOptions.Hash, err)))
			return false
		}
	} else {
		cloneOptions := git.CloneOptions{
			URL:      clone_url,
			Progress: logUploader,
			Depth:    clone_depth,
		}
		if !is_tag {
			cloneOptions.Tags = git.NoTags
		}

		if is_tag {
			cloneOptions.SingleBranch = true
			cloneOptions.ReferenceName = plumbing.ReferenceName(fmt.Sprintf("refs/tags/%s", tag))
		} else {
			cloneOptions.SingleBranch = true
			cloneOptions.ReferenceName = plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", branch))
		}
		logUploader.Write([]byte(fmt.Sprintf("\nCloning %s...\n", cloneOptions.ReferenceName)))

		repo, err = git.PlainCloneContext(ctx, working_dir, false, &cloneOptions)

		if err != nil && retryableCloneError(err) {
			logUploader.Write([]byte(fmt.Sprintf("\nRetryable error '%s' while cloning! Trying again...", err)))
			os.RemoveAll(working_dir)
			EnsureFolderExists(working_dir)
			repo, err = git.PlainClone(working_dir, false, &cloneOptions)
		}

		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "timeout") || strings.Contains(strings.ToLower(err.Error()), "timed out") {
				logUploader.Write([]byte("\nFailed to clone because of a timeout from Git server!"))
			} else {
				logUploader.Write([]byte(fmt.Sprintf("\nFailed to clone: %s!", err)))
			}
			return false
		}
	}

	ref, err := repo.Head()
	if err != nil {
		logUploader.Write([]byte("\nFailed to get HEAD information!"))
		return false
	}

	if ref.Hash() != plumbing.NewHash(change) {
		logUploader.Write([]byte(fmt.Sprintf("\nHEAD is at %s.", ref.Hash())))
		logUploader.Write([]byte(fmt.Sprintf("\nHard resetting to %s...", change)))

		workTree, err := repo.Worktree()
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to get work tree: %s!", err)))
			return false
		}

		err = workTree.Reset(&git.ResetOptions{
			Commit: plumbing.NewHash(change),
			Mode:   git.HardReset,
		})
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to force reset to %s: %s!", change, err)))
			return false
		}
	}

	if is_clone_modules {
		logUploader.Write([]byte("\nUpdating submodules..."))

		workTree, err := repo.Worktree()
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to get work tree: %s!", err)))
			return false
		}

		submodules, err := workTree.Submodules()
		if err != nil {
			logUploader.Write([]byte(fmt.Sprintf("\nFailed to get submodules: %s!", err)))
			return false
		}

		opts := &git.SubmoduleUpdateOptions{
			Init:              true,
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
		}

		for _, sub := range submodules {
			if err := sub.UpdateContext(ctx, opts); err != nil {
				logUploader.Write([]byte(fmt.Sprintf("\nFailed to update submodule %q: %s!",
					sub.Config().Name, err)))
				return false
			}
		}

		logUploader.Write([]byte("\nSucessfully updated submodules!"))
	}

	logUploader.Write([]byte(fmt.Sprintf("\nChecked out %s on %s branch.", change, branch)))
	logUploader.Write([]byte("\nSuccessfully cloned!"))

	return true
}

func (executor *Executor) shouldKillProcesses() bool {
	_, shouldNotKillProcesses := executor.env.Lookup("CIRRUS_ESCAPING_PROCESSES")

	return !shouldNotKillProcesses
}

func retryableCloneError(err error) bool {
	if err == nil {
		return false
	}
	errorMessage := strings.ToLower(err.Error())
	if strings.Contains(errorMessage, "timeout") {
		return true
	}
	if strings.Contains(errorMessage, "tls") {
		return true
	}
	if strings.Contains(errorMessage, "connection") {
		return true
	}
	if strings.Contains(errorMessage, "authentication") {
		return true
	}
	if strings.Contains(errorMessage, "not found") {
		return true
	}
	return false
}

func (executor *Executor) reportError(message string) {
	request := api.ReportAgentProblemRequest{
		TaskIdentification: executor.taskIdentification,
		Message:            message,
	}
	_, _ = client.CirrusClient.ReportAgentError(context.Background(), &request)
}
