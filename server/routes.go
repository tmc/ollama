package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/jmorganca/ollama/api"
	"github.com/jmorganca/ollama/llm"
	"github.com/jmorganca/ollama/parser"
	"github.com/jmorganca/ollama/version"
)

var mode string = gin.DebugMode

func init() {
	switch mode {
	case gin.DebugMode:
	case gin.ReleaseMode:
	case gin.TestMode:
	default:
		mode = gin.DebugMode
	}

	gin.SetMode(mode)
}

var loaded struct {
	mu sync.Mutex

	runner llm.LLM

	expireAt    time.Time
	expireTimer *time.Timer

	*Model
	*api.Options
}

var defaultSessionDuration = 5 * time.Minute

// load a model into memory if it is not already loaded, it is up to the caller to lock loaded.mu before calling this function
func load(c *gin.Context, modelName string, reqOpts map[string]interface{}, sessionDuration time.Duration) (*Model, error) {
	model, err := GetModel(modelName)
	if err != nil {
		return nil, err
	}

	workDir := c.GetString("workDir")

	opts := api.DefaultOptions()
	if err := opts.FromMap(model.Options); err != nil {
		log.Printf("could not load model options: %v", err)
		return nil, err
	}

	if err := opts.FromMap(reqOpts); err != nil {
		return nil, err
	}

	ctx := c.Request.Context()

	// check if the loaded model is still running in a subprocess, in case something unexpected happened
	if loaded.runner != nil {
		if err := loaded.runner.Ping(ctx); err != nil {
			log.Print("loaded llm process not responding, closing now")
			// the subprocess is no longer running, so close it
			loaded.runner.Close()
			loaded.runner = nil
			loaded.Model = nil
			loaded.Options = nil
		}
	}

	needLoad := loaded.runner == nil || // is there a model loaded?
		loaded.ModelPath != model.ModelPath || // has the base model changed?
		!reflect.DeepEqual(loaded.AdapterPaths, model.AdapterPaths) || // have the adapters changed?
		!reflect.DeepEqual(loaded.Options.Runner, opts.Runner) // have the runner options changed?

	if needLoad {
		if loaded.runner != nil {
			log.Println("changing loaded model")
			loaded.runner.Close()
			loaded.runner = nil
			loaded.Model = nil
			loaded.Options = nil
		}

		llmRunner, err := llm.New(workDir, model.ModelPath, model.AdapterPaths, model.ProjectorPaths, opts)
		if err != nil {
			// some older models are not compatible with newer versions of llama.cpp
			// show a generalized compatibility error until there is a better way to
			// check for model compatibility
			if strings.Contains(err.Error(), "failed to load model") {
				err = fmt.Errorf("%v: this model may be incompatible with your version of Ollama. If you previously pulled this model, try updating it by running `ollama pull %s`", err, model.ShortName)
			}

			return nil, err
		}

		loaded.Model = model
		loaded.runner = llmRunner
		loaded.Options = &opts
	}

	// update options for the loaded llm
	// TODO(mxyng): this isn't thread safe, but it should be fine for now
	loaded.runner.SetOptions(opts)

	loaded.expireAt = time.Now().Add(sessionDuration)

	if loaded.expireTimer == nil {
		loaded.expireTimer = time.AfterFunc(sessionDuration, func() {
			loaded.mu.Lock()
			defer loaded.mu.Unlock()

			if time.Now().Before(loaded.expireAt) {
				return
			}

			if loaded.runner != nil {
				loaded.runner.Close()
			}

			loaded.runner = nil
			loaded.Model = nil
			loaded.Options = nil
		})
	}

	loaded.expireTimer.Reset(sessionDuration)
	return model, nil
}

func GenerateHandler(c *gin.Context) {
	loaded.mu.Lock()
	defer loaded.mu.Unlock()

	checkpointStart := time.Now()

	var req api.GenerateRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// validate the request
	switch {
	case req.Model == "":
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	case len(req.Format) > 0 && req.Format != "json":
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "format must be json"})
		return
	case req.Raw && (req.Template != "" || req.System != "" || len(req.Context) > 0):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "raw mode does not support template, system, or context"})
		return
	}

	sessionDuration := defaultSessionDuration
	model, err := load(c, req.Model, req.Options, sessionDuration)
	if err != nil {
		var pErr *fs.PathError
		switch {
		case errors.As(err, &pErr):
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
		case errors.Is(err, api.ErrInvalidOpts):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	// an empty request loads the model
	if req.Prompt == "" && req.Template == "" && req.System == "" {
		c.JSON(http.StatusOK, api.GenerateResponse{
			CreatedAt:          time.Now().UTC(),
			Model:              req.Model,
			ModelConfiguration: model.Config.ModelConfiguration,
			Done:               true})
		return
	}

	checkpointLoaded := time.Now()

	var prompt string
	var promptVars PromptVars
	switch {
	case req.Raw:
		prompt = req.Prompt
	case req.Prompt != "":
		if req.Template != "" {
			// override the default model template
			model.Template = req.Template
		}

		var rebuild strings.Builder
		if req.Context != nil {
			// TODO: context is deprecated, at some point the context logic within this conditional should be removed
			prevCtx, err := loaded.runner.Decode(c.Request.Context(), req.Context)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			// Remove leading spaces from prevCtx if present
			prevCtx = strings.TrimPrefix(prevCtx, " ")
			rebuild.WriteString(prevCtx)
		}
		promptVars = PromptVars{
			System: req.System,
			Prompt: req.Prompt,
			First:  len(req.Context) == 0,
		}
		p, err := model.PreResponsePrompt(promptVars)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rebuild.WriteString(p)
		prompt = rebuild.String()
	}

	ch := make(chan any)
	var generated strings.Builder
	go func() {
		defer close(ch)

		fn := func(r llm.PredictResult) {
			// Update model expiration
			loaded.expireAt = time.Now().Add(sessionDuration)
			loaded.expireTimer.Reset(sessionDuration)

			// Build up the full response
			if _, err := generated.WriteString(r.Content); err != nil {
				ch <- gin.H{"error": err.Error()}
				return
			}

			resp := api.GenerateResponse{
				Model:              r.Model,
				ModelConfiguration: model.Config.ModelConfiguration,
				CreatedAt:          r.CreatedAt,
				Done:               r.Done,
				Response:           r.Content,
				Metrics: api.Metrics{
					TotalDuration:      r.TotalDuration,
					LoadDuration:       r.LoadDuration,
					PromptEvalCount:    r.PromptEvalCount,
					PromptEvalDuration: r.PromptEvalDuration,
					EvalCount:          r.EvalCount,
					EvalDuration:       r.EvalDuration,
				},
			}

			if r.Done && !req.Raw {
				// append the generated text to the history and template it if needed
				promptVars.Response = generated.String()
				result, err := model.PostResponseTemplate(promptVars)
				if err != nil {
					ch <- gin.H{"error": err.Error()}
					return
				}
				embd, err := loaded.runner.Encode(c.Request.Context(), prompt+result)
				if err != nil {
					ch <- gin.H{"error": err.Error()}
					return
				}
				resp.Context = embd
			}

			ch <- resp
		}

		// Start prediction
		predictReq := llm.PredictOpts{
			Model:            model.Name,
			Prompt:           prompt,
			Format:           req.Format,
			CheckpointStart:  checkpointStart,
			CheckpointLoaded: checkpointLoaded,
		}
		if err := loaded.runner.Predict(c.Request.Context(), predictReq, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		// Wait for the channel to close
		var r api.GenerateResponse
		var sb strings.Builder
		for resp := range ch {
			var ok bool
			if r, ok = resp.(api.GenerateResponse); !ok {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			sb.WriteString(r.Response)
		}
		r.Response = sb.String()
		c.JSON(http.StatusOK, r)
		return
	}

	streamResponse(c, ch)
}

func EmbeddingHandler(c *gin.Context) {
	loaded.mu.Lock()
	defer loaded.mu.Unlock()

	var req api.EmbeddingRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Model == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	sessionDuration := defaultSessionDuration
	_, err = load(c, req.Model, req.Options, sessionDuration)
	if err != nil {
		var pErr *fs.PathError
		switch {
		case errors.As(err, &pErr):
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
		case errors.Is(err, api.ErrInvalidOpts):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	if !loaded.Options.EmbeddingOnly {
		c.JSON(http.StatusBadRequest, gin.H{"error": "embedding option must be set to true"})
		return
	}

	embedding, err := loaded.runner.Embedding(c.Request.Context(), req.Prompt)
	if err != nil {
		log.Printf("embedding generation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate embedding"})
		return
	}

	resp := api.EmbeddingResponse{
		Embedding: embedding,
	}
	c.JSON(http.StatusOK, resp)
}

func PullModelHandler(c *gin.Context) {
	var req api.PullRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &RegistryOptions{
			Insecure: req.Insecure,
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := PullModel(ctx, req.Name, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func PushModelHandler(c *gin.Context) {
	var req api.PushRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &RegistryOptions{
			Insecure: req.Insecure,
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := PushModel(ctx, req.Name, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func CreateModelHandler(c *gin.Context) {
	var req api.CreateRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if err := ParseModelPath(req.Name).Validate(); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Path == "" && req.Modelfile == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "path or modelfile are required"})
		return
	}

	var modelfile io.Reader = strings.NewReader(req.Modelfile)
	if req.Path != "" && req.Modelfile == "" {
		mf, err := os.Open(req.Path)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("error reading modelfile: %s", err)})
			return
		}
		defer mf.Close()

		modelfile = mf
	}

	commands, err := parser.Parse(modelfile)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(resp api.ProgressResponse) {
			ch <- resp
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := CreateModel(ctx, req.Name, filepath.Dir(req.Path), commands, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func DeleteModelHandler(c *gin.Context) {
	var req api.DeleteRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if err := DeleteModel(req.Name); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Name)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	manifestsPath, err := GetManifestPath()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := PruneDirectory(manifestsPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, nil)
}

func ShowModelHandler(c *gin.Context) {
	var req api.ShowRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	resp, err := GetModelInfo(req.Name)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Name)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, resp)
}

func GetModelInfo(name string) (*api.ShowResponse, error) {
	model, err := GetModel(name)
	if err != nil {
		return nil, err
	}

	resp := &api.ShowResponse{
		License:  strings.Join(model.License, "\n"),
		System:   model.System,
		Template: model.Template,
	}

	mf, err := ShowModelfile(model)
	if err != nil {
		return nil, err
	}

	resp.Modelfile = mf

	var params []string
	cs := 30
	for k, v := range model.Options {
		switch val := v.(type) {
		case string:
			params = append(params, fmt.Sprintf("%-*s %s", cs, k, val))
		case int:
			params = append(params, fmt.Sprintf("%-*s %s", cs, k, strconv.Itoa(val)))
		case float64:
			params = append(params, fmt.Sprintf("%-*s %s", cs, k, strconv.FormatFloat(val, 'f', 0, 64)))
		case bool:
			params = append(params, fmt.Sprintf("%-*s %s", cs, k, strconv.FormatBool(val)))
		case []interface{}:
			for _, nv := range val {
				switch nval := nv.(type) {
				case string:
					params = append(params, fmt.Sprintf("%-*s %s", cs, k, nval))
				case int:
					params = append(params, fmt.Sprintf("%-*s %s", cs, k, strconv.Itoa(nval)))
				case float64:
					params = append(params, fmt.Sprintf("%-*s %s", cs, k, strconv.FormatFloat(nval, 'f', 0, 64)))
				case bool:
					params = append(params, fmt.Sprintf("%-*s %s", cs, k, strconv.FormatBool(nval)))
				}
			}
		}
	}
	resp.Parameters = strings.Join(params, "\n")

	return resp, nil
}

func ListModelsHandler(c *gin.Context) {
	models := make([]api.ModelResponse, 0)
	fp, err := GetManifestPath()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	walkFunc := func(path string, info os.FileInfo, _ error) error {
		if !info.IsDir() {
			dir, file := filepath.Split(path)
			dir = strings.Trim(strings.TrimPrefix(dir, fp), string(os.PathSeparator))
			tag := strings.Join([]string{dir, file}, ":")

			mp := ParseModelPath(tag)
			manifest, digest, err := GetManifest(mp)
			if err != nil {
				log.Printf("skipping file: %s", fp)
				return nil
			}

			models = append(models, api.ModelResponse{
				Name:       mp.GetShortTagname(),
				Size:       manifest.GetTotalSize(),
				Digest:     digest,
				ModifiedAt: info.ModTime(),
			})
		}

		return nil
	}

	if err := filepath.Walk(fp, walkFunc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, api.ListResponse{Models: models})
}

func CopyModelHandler(c *gin.Context) {
	var req api.CopyRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Source == "" || req.Destination == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "source add destination are required"})
		return
	}

	if err := ParseModelPath(req.Destination).Validate(); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := CopyModel(req.Source, req.Destination); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Source)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
}

func HeadBlobHandler(c *gin.Context) {
	path, err := GetBlobsPath(c.Param("digest"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if _, err := os.Stat(path); err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("blob %q not found", c.Param("digest"))})
		return
	}

	c.Status(http.StatusOK)
}

func CreateBlobHandler(c *gin.Context) {
	layer, err := NewLayer(c.Request.Body, "")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if layer.Digest != c.Param("digest") {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("digest mismatch, expected %q, got %q", c.Param("digest"), layer.Digest)})
		return
	}

	if _, err := layer.Commit(); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Status(http.StatusCreated)
}

var defaultAllowOrigins = []string{
	"localhost",
	"127.0.0.1",
	"0.0.0.0",
}

func Serve(ln net.Listener, allowOrigins []string) error {
	if noprune := os.Getenv("OLLAMA_NOPRUNE"); noprune == "" {
		// clean up unused layers and manifests
		if err := PruneLayers(); err != nil {
			return err
		}

		manifestsPath, err := GetManifestPath()
		if err != nil {
			return err
		}

		if err := PruneDirectory(manifestsPath); err != nil {
			return err
		}
	}

	config := cors.DefaultConfig()
	config.AllowWildcard = true

	config.AllowOrigins = allowOrigins
	for _, allowOrigin := range defaultAllowOrigins {
		config.AllowOrigins = append(config.AllowOrigins,
			fmt.Sprintf("http://%s", allowOrigin),
			fmt.Sprintf("https://%s", allowOrigin),
			fmt.Sprintf("http://%s:*", allowOrigin),
			fmt.Sprintf("https://%s:*", allowOrigin),
		)
	}

	workDir, err := os.MkdirTemp("", "ollama")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	r := gin.Default()
	r.Use(
		cors.New(config),
		func(c *gin.Context) {
			c.Set("workDir", workDir)
			c.Next()
		},
	)

	r.POST("/api/pull", PullModelHandler)
	r.POST("/api/generate", GenerateHandler)
	r.POST("/api/chat", ChatHandler)
	r.POST("/api/embeddings", EmbeddingHandler)
	r.POST("/api/create", CreateModelHandler)
	r.POST("/api/push", PushModelHandler)
	r.POST("/api/copy", CopyModelHandler)
	r.DELETE("/api/delete", DeleteModelHandler)
	r.POST("/api/show", ShowModelHandler)
	r.POST("/api/blobs/:digest", CreateBlobHandler)
	r.HEAD("/api/blobs/:digest", HeadBlobHandler)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r.Handle(method, "/", func(c *gin.Context) {
			c.String(http.StatusOK, "Ollama is running")
		})

		r.Handle(method, "/api/tags", ListModelsHandler)
		r.Handle(method, "/api/version", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"version": version.Version})
		})
	}

	log.Printf("Listening on %s (version %s)", ln.Addr(), version.Version)
	s := &http.Server{
		Handler: r,
	}

	// listen for a ctrl+c and stop any loaded llm
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		if loaded.runner != nil {
			loaded.runner.Close()
		}
		os.RemoveAll(workDir)
		os.Exit(0)
	}()

	if runtime.GOOS == "linux" {
		// check compatibility to log warnings
		if _, err := llm.CheckVRAM(); err != nil {
			log.Printf(err.Error())
		}
	}

	return s.Serve(ln)
}

func waitForStream(c *gin.Context, ch chan interface{}) {
	c.Header("Content-Type", "application/json")
	for resp := range ch {
		switch r := resp.(type) {
		case api.ProgressResponse:
			if r.Status == "success" {
				c.JSON(http.StatusOK, r)
				return
			}
		case gin.H:
			if errorMsg, ok := r["error"].(string); ok {
				c.JSON(http.StatusInternalServerError, gin.H{"error": errorMsg})
				return
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error format in progress response"})
				return
			}
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected progress response"})
			return
		}
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected end of progress response"})
}

func streamResponse(c *gin.Context, ch chan any) {
	c.Header("Content-Type", "application/x-ndjson")
	c.Stream(func(w io.Writer) bool {
		val, ok := <-ch
		if !ok {
			return false
		}

		bts, err := json.Marshal(val)
		if err != nil {
			log.Printf("streamResponse: json.Marshal failed with %s", err)
			return false
		}

		// Delineate chunks with new-line delimiter
		bts = append(bts, '\n')
		if _, err := w.Write(bts); err != nil {
			log.Printf("streamResponse: w.Write failed with %s", err)
			return false
		}

		return true
	})
}

func ChatHandler(c *gin.Context) {
	loaded.mu.Lock()
	defer loaded.mu.Unlock()

	checkpointStart := time.Now()

	var req api.ChatRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// validate the request
	switch {
	case req.Model == "":
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	case len(req.Format) > 0 && req.Format != "json":
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "format must be json"})
		return
	}

	sessionDuration := defaultSessionDuration
	model, err := load(c, req.Model, req.Options, sessionDuration)
	if err != nil {
		var pErr *fs.PathError
		switch {
		case errors.As(err, &pErr):
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
		case errors.Is(err, api.ErrInvalidOpts):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	// an empty request loads the model
	if len(req.Messages) == 0 {
		c.JSON(http.StatusOK, api.ChatResponse{CreatedAt: time.Now().UTC(), Model: req.Model, Done: true})
		return
	}

	checkpointLoaded := time.Now()

	prompt, err := model.ChatPrompt(req.Messages)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ch := make(chan any)

	go func() {
		defer close(ch)

		fn := func(r llm.PredictResult) {
			// Update model expiration
			loaded.expireAt = time.Now().Add(sessionDuration)
			loaded.expireTimer.Reset(sessionDuration)

			resp := api.ChatResponse{
				Model:     r.Model,
				CreatedAt: r.CreatedAt,
				Done:      r.Done,
				Metrics: api.Metrics{
					TotalDuration:      r.TotalDuration,
					LoadDuration:       r.LoadDuration,
					PromptEvalCount:    r.PromptEvalCount,
					PromptEvalDuration: r.PromptEvalDuration,
					EvalCount:          r.EvalCount,
					EvalDuration:       r.EvalDuration,
				},
			}

			if !r.Done {
				resp.Message = &api.Message{Role: "assistant", Content: r.Content}
			}

			ch <- resp
		}

		// Start prediction
		predictReq := llm.PredictOpts{
			Model:            model.Name,
			Prompt:           prompt,
			Format:           req.Format,
			CheckpointStart:  checkpointStart,
			CheckpointLoaded: checkpointLoaded,
		}
		if err := loaded.runner.Predict(c.Request.Context(), predictReq, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		// Wait for the channel to close
		var r api.ChatResponse
		var sb strings.Builder
		for resp := range ch {
			var ok bool
			if r, ok = resp.(api.ChatResponse); !ok {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			if r.Message != nil {
				sb.WriteString(r.Message.Content)
			}
		}
		r.Message = &api.Message{Role: "assistant", Content: sb.String()}
		c.JSON(http.StatusOK, r)
		return
	}

	streamResponse(c, ch)
}
