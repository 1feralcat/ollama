package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"golang.org/x/exp/slices"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/gpu"
	"github.com/ollama/ollama/llm"
	"github.com/ollama/ollama/openai"
	"github.com/ollama/ollama/types/model"
	"github.com/ollama/ollama/version"
)

var mode string = gin.DebugMode

type Server struct {
	addr  net.Addr
	sched *Scheduler
}

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

var defaultSessionDuration = 5 * time.Minute

func modelOptions(model *Model, requestOpts map[string]interface{}) (api.Options, error) {
	opts := api.DefaultOptions()
	if err := opts.FromMap(model.Options); err != nil {
		return api.Options{}, err
	}

	if err := opts.FromMap(requestOpts); err != nil {
		return api.Options{}, err
	}

	return opts, nil
}

func isSupportedImageType(image []byte) bool {
	contentType := http.DetectContentType(image)
	allowedTypes := []string{"image/jpeg", "image/jpg", "image/png"}
	return slices.Contains(allowedTypes, contentType)
}

func (s *Server) GenerateHandler(c *gin.Context) {

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

	for _, img := range req.Images {
		if !isSupportedImageType(img) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "unsupported image format"})
			return
		}
	}

	model, err := GetModel(req.Model)
	if err != nil {
		var pErr *fs.PathError
		if errors.As(err, &pErr) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if model.IsEmbedding() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "embedding models do not support generate"})
		return
	}

	opts, err := modelOptions(model, req.Options)
	if err != nil {
		if errors.Is(err, api.ErrInvalidOpts) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var sessionDuration time.Duration
	if req.KeepAlive == nil {
		sessionDuration = getDefaultSessionDuration()
	} else {
		sessionDuration = req.KeepAlive.Duration
	}

	rCh, eCh := s.sched.GetRunner(c.Request.Context(), model, opts, sessionDuration)
	var runner *runnerRef
	select {
	case runner = <-rCh:
	case err = <-eCh:
		handleErrorResponse(c, err)
		return
	}

	// an empty request loads the model
	// note: for a short while template was used in lieu
	// of `raw` mode so we need to check for it too
	if req.Prompt == "" && req.Template == "" && req.System == "" {
		c.JSON(http.StatusOK, api.GenerateResponse{
			CreatedAt: time.Now().UTC(),
			Model:     req.Model,
			Done:      true,
		})
		return
	}

	checkpointLoaded := time.Now()

	var prompt string
	switch {
	case req.Raw:
		prompt = req.Prompt
	case req.Prompt != "":
		if req.Template == "" {
			req.Template = model.Template
		}

		if req.System == "" {
			req.System = model.System
		}

		slog.Debug("generate handler", "prompt", req.Prompt)
		slog.Debug("generate handler", "template", req.Template)
		slog.Debug("generate handler", "system", req.System)

		var sb strings.Builder
		for i := range req.Images {
			fmt.Fprintf(&sb, "[img-%d] ", i)
		}

		sb.WriteString(req.Prompt)

		p, err := Prompt(req.Template, req.System, sb.String(), "", true)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		sb.Reset()
		if req.Context != nil {
			prev, err := runner.llama.Detokenize(c.Request.Context(), req.Context)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			sb.WriteString(prev)
		}

		sb.WriteString(p)

		prompt = sb.String()
	}

	slog.Debug("generate handler", "prompt", prompt)

	ch := make(chan any)
	var generated strings.Builder
	go func() {
		defer close(ch)

		fn := func(r llm.CompletionResponse) {
			// Build up the full response
			if _, err := generated.WriteString(r.Content); err != nil {
				ch <- gin.H{"error": err.Error()}
				return
			}

			resp := api.GenerateResponse{
				Model:     req.Model,
				CreatedAt: time.Now().UTC(),
				Done:      r.Done,
				Response:  r.Content,
				Metrics: api.Metrics{
					PromptEvalCount:    r.PromptEvalCount,
					PromptEvalDuration: r.PromptEvalDuration,
					EvalCount:          r.EvalCount,
					EvalDuration:       r.EvalDuration,
				},
			}

			if r.Done {
				resp.TotalDuration = time.Since(checkpointStart)
				resp.LoadDuration = checkpointLoaded.Sub(checkpointStart)

				if !req.Raw {
					p, err := Prompt(req.Template, req.System, req.Prompt, generated.String(), false)
					if err != nil {
						c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
						return
					}

					// TODO (jmorganca): encode() should not strip special tokens
					tokens, err := runner.llama.Tokenize(c.Request.Context(), p)
					if err != nil {
						ch <- gin.H{"error": err.Error()}
						return
					}

					resp.Context = append(req.Context, tokens...)
				}
			}

			ch <- resp
		}

		var images []llm.ImageData
		for i := range req.Images {
			images = append(images, llm.ImageData{
				ID:   i,
				Data: req.Images[i],
			})
		}

		// Start prediction
		req := llm.CompletionRequest{
			Prompt:  prompt,
			Format:  req.Format,
			Images:  images,
			Options: opts,
		}
		if err := runner.llama.Completion(c.Request.Context(), req, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		// Accumulate responses into the final response
		var final api.GenerateResponse
		var sb strings.Builder
		for resp := range ch {
			switch r := resp.(type) {
			case api.GenerateResponse:
				sb.WriteString(r.Response)
				final = r
			case gin.H:
				if errorMsg, ok := r["error"].(string); ok {
					c.JSON(http.StatusInternalServerError, gin.H{"error": errorMsg})
					return
				} else {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error format in response"})
					return
				}
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error"})
				return
			}
		}

		final.Response = sb.String()
		c.JSON(http.StatusOK, final)
		return
	}

	streamResponse(c, ch)
}

func getDefaultSessionDuration() time.Duration {
	if t, exists := os.LookupEnv("OLLAMA_KEEP_ALIVE"); exists {
		v, err := strconv.Atoi(t)
		if err != nil {
			d, err := time.ParseDuration(t)
			if err != nil {
				return defaultSessionDuration
			}

			if d < 0 {
				return time.Duration(math.MaxInt64)
			}

			return d
		}

		d := time.Duration(v) * time.Second
		if d < 0 {
			return time.Duration(math.MaxInt64)
		}
		return d
	}

	return defaultSessionDuration
}

func (s *Server) EmbeddingsHandler(c *gin.Context) {
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

	model, err := GetModel(req.Model)
	if err != nil {
		var pErr *fs.PathError
		if errors.As(err, &pErr) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	opts, err := modelOptions(model, req.Options)
	if err != nil {
		if errors.Is(err, api.ErrInvalidOpts) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var sessionDuration time.Duration
	if req.KeepAlive == nil {
		sessionDuration = getDefaultSessionDuration()
	} else {
		sessionDuration = req.KeepAlive.Duration
	}

	rCh, eCh := s.sched.GetRunner(c.Request.Context(), model, opts, sessionDuration)
	var runner *runnerRef
	select {
	case runner = <-rCh:
	case err = <-eCh:
		handleErrorResponse(c, err)
		return
	}

	// an empty request loads the model
	if req.Prompt == "" {
		c.JSON(http.StatusOK, api.EmbeddingResponse{Embedding: []float64{}})
		return
	}

	embedding, err := runner.llama.Embedding(c.Request.Context(), req.Prompt)
	if err != nil {
		slog.Info(fmt.Sprintf("embedding generation failed: %v", err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate embedding"})
		return
	}

	resp := api.EmbeddingResponse{
		Embedding: embedding,
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) PullModelHandler(c *gin.Context) {
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

	var model string
	if req.Model != "" {
		model = req.Model
	} else if req.Name != "" {
		model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &registryOptions{
			Insecure: req.Insecure,
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := PullModel(ctx, model, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func (s *Server) PushModelHandler(c *gin.Context) {
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

	var model string
	if req.Model != "" {
		model = req.Model
	} else if req.Name != "" {
		model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &registryOptions{
			Insecure: req.Insecure,
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := PushModel(ctx, model, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func (s *Server) CreateModelHandler(c *gin.Context) {
	var req api.CreateRequest
	if err := c.ShouldBindJSON(&req); errors.Is(err, io.EOF) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	} else if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	name := model.ParseName(cmp.Or(req.Model, req.Name))
	if !name.IsValid() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid model name"})
		return
	}

	if req.Path == "" && req.Modelfile == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "path or modelfile are required"})
		return
	}

	var r io.Reader = strings.NewReader(req.Modelfile)
	if req.Path != "" && req.Modelfile == "" {
		f, err := os.Open(req.Path)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("error reading modelfile: %s", err)})
			return
		}
		defer f.Close()

		r = f
	}

	modelfile, err := model.ParseFile(r)
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

		if err := CreateModel(ctx, name.String(), filepath.Dir(req.Path), req.Quantization, modelfile, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func (s *Server) DeleteModelHandler(c *gin.Context) {
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

	var model string
	if req.Model != "" {
		model = req.Model
	} else if req.Name != "" {
		model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	if err := DeleteModel(model); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", model)})
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

func (s *Server) ShowModelHandler(c *gin.Context) {
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

	if req.Model != "" {
		// noop
	} else if req.Name != "" {
		req.Model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	resp, err := GetModelInfo(req)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Model)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, resp)
}

func GetModelInfo(req api.ShowRequest) (*api.ShowResponse, error) {
	model, err := GetModel(req.Model)
	if err != nil {
		return nil, err
	}

	modelDetails := api.ModelDetails{
		ParentModel:       model.ParentModel,
		Format:            model.Config.ModelFormat,
		Family:            model.Config.ModelFamily,
		Families:          model.Config.ModelFamilies,
		ParameterSize:     model.Config.ModelType,
		QuantizationLevel: model.Config.FileType,
	}

	if req.System != "" {
		model.System = req.System
	}

	if req.Template != "" {
		model.Template = req.Template
	}

	msgs := make([]api.Message, 0)
	for _, msg := range model.Messages {
		msgs = append(msgs, api.Message{Role: msg.Role, Content: msg.Content})
	}

	resp := &api.ShowResponse{
		License:  strings.Join(model.License, "\n"),
		System:   model.System,
		Template: model.Template,
		Details:  modelDetails,
		Messages: msgs,
	}

	var params []string
	cs := 30
	for k, v := range model.Options {
		switch val := v.(type) {
		case []interface{}:
			for _, nv := range val {
				params = append(params, fmt.Sprintf("%-*s %#v", cs, k, nv))
			}
		default:
			params = append(params, fmt.Sprintf("%-*s %#v", cs, k, v))
		}
	}
	resp.Parameters = strings.Join(params, "\n")

	for k, v := range req.Options {
		if _, ok := req.Options[k]; ok {
			model.Options[k] = v
		}
	}

	var sb strings.Builder
	fmt.Fprintln(&sb, "# Modelfile generate by \"ollama show\"")
	fmt.Fprintln(&sb, "# To build a new Modelfile based on this, replace FROM with:")
	fmt.Fprintf(&sb, "# FROM %s\n\n", model.ShortName)
	fmt.Fprint(&sb, model.String())
	resp.Modelfile = sb.String()

	return resp, nil
}

func (s *Server) ListModelsHandler(c *gin.Context) {
	models := make([]api.ModelResponse, 0)
	manifestsPath, err := GetManifestPath()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	modelResponse := func(modelName string) (api.ModelResponse, error) {
		model, err := GetModel(modelName)
		if err != nil {
			return api.ModelResponse{}, err
		}

		modelDetails := api.ModelDetails{
			Format:            model.Config.ModelFormat,
			Family:            model.Config.ModelFamily,
			Families:          model.Config.ModelFamilies,
			ParameterSize:     model.Config.ModelType,
			QuantizationLevel: model.Config.FileType,
		}

		return api.ModelResponse{
			Model:   model.ShortName,
			Name:    model.ShortName,
			Size:    model.Size,
			Digest:  model.Digest,
			Details: modelDetails,
		}, nil
	}

	walkFunc := func(path string, info os.FileInfo, _ error) error {
		if !info.IsDir() {
			path, tag := filepath.Split(path)
			model := strings.Trim(strings.TrimPrefix(path, manifestsPath), string(os.PathSeparator))
			modelPath := strings.Join([]string{model, tag}, ":")
			canonicalModelPath := strings.ReplaceAll(modelPath, string(os.PathSeparator), "/")

			resp, err := modelResponse(canonicalModelPath)
			if err != nil {
				slog.Info(fmt.Sprintf("skipping file: %s", canonicalModelPath))
				// nolint: nilerr
				return nil
			}

			resp.ModifiedAt = info.ModTime()
			models = append(models, resp)
		}

		return nil
	}

	if err := filepath.Walk(manifestsPath, walkFunc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, api.ListResponse{Models: models})
}

func (s *Server) CopyModelHandler(c *gin.Context) {
	var r api.CopyRequest
	if err := c.ShouldBindJSON(&r); errors.Is(err, io.EOF) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	} else if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	src := model.ParseName(r.Source)
	if !src.IsValid() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("source %q is invalid", r.Source)})
		return
	}

	dst := model.ParseName(r.Destination)
	if !dst.IsValid() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("destination %q is invalid", r.Source)})
		return
	}

	if err := CopyModel(src, dst); errors.Is(err, os.ErrNotExist) {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model %q not found", r.Source)})
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func (s *Server) HeadBlobHandler(c *gin.Context) {
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

func (s *Server) CreateBlobHandler(c *gin.Context) {
	path, err := GetBlobsPath(c.Param("digest"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	_, err = os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// noop
	case err != nil:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	default:
		c.Status(http.StatusOK)
		return
	}

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

func (s *Server) WebsiteHandler(c *gin.Context) {
	basePath := os.Getenv("OLLAMA_WEBSITE")
	if basePath == "" {
		slog.Error("OLLAMA_WEBSITE is not set")
		c.Status(http.StatusNotFound)
		return
	}

	filename := c.Param("filename")
	if filename == "" || filename == "/" {
		filename = "index.html"
	} else {
		filename = strings.TrimPrefix(filename, "/")
	}

	filePath := filepath.Clean(filepath.Join(basePath, filename))
	if !strings.HasPrefix(filePath, basePath) {
		slog.Error("path traversal attempt detected", "filename", filename, "filePath", filePath)
		c.Status(http.StatusNotFound)
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		slog.Error("error opening file", "error", err)
		if os.IsNotExist(err) {
			c.Status(http.StatusNotFound)
			return
		}
		c.String(http.StatusInternalServerError, "Error reading file")
		return
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		slog.Error("error getting file info", "error", err)
		c.String(http.StatusInternalServerError, "Error getting file info")
		return
	}
	c.Header("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))

	contentType := mime.TypeByExtension(filepath.Ext(filename))
	if contentType != "" {
		c.Header("Content-Type", contentType)
	}

	c.Status(http.StatusOK)
	_, err = io.Copy(c.Writer, file)
	if err != nil {
		slog.Error("error serving file", "error", err)
		c.String(http.StatusInternalServerError, "Error serving file")
		return
	}
}
var defaultAllowOrigins = []string{
	"localhost",
	"127.0.0.1",
	"0.0.0.0",
}

func isLocalIP(ip netip.Addr) bool {
	if interfaces, err := net.Interfaces(); err == nil {
		for _, iface := range interfaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}

			for _, a := range addrs {
				if parsed, _, err := net.ParseCIDR(a.String()); err == nil {
					if parsed.String() == ip.String() {
						return true
					}
				}
			}
		}
	}

	return false
}

func allowedHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}

	if hostname, err := os.Hostname(); err == nil && host == hostname {
		return true
	}

	var tlds = []string{
		"localhost",
		"local",
		"internal",
	}

	// check if the host is a local TLD
	for _, tld := range tlds {
		if strings.HasSuffix(host, "."+tld) {
			return true
		}
	}

	return false
}

func allowedHostsMiddleware(addr net.Addr) gin.HandlerFunc {
	return func(c *gin.Context) {
		if addr == nil {
			c.Next()
			return
		}

		if addr, err := netip.ParseAddrPort(addr.String()); err == nil && !addr.Addr().IsLoopback() {
			c.Next()
			return
		}

		host, _, err := net.SplitHostPort(c.Request.Host)
		if err != nil {
			host = c.Request.Host
		}

		if addr, err := netip.ParseAddr(host); err == nil {
			if addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() || isLocalIP(addr) {
				c.Next()
				return
			}
		}

		if allowedHost(host) {
			c.Next()
			return
		}

		c.AbortWithStatus(http.StatusForbidden)
	}
}

func (s *Server) GenerateRoutes() http.Handler {
	config := cors.DefaultConfig()
	config.AllowWildcard = true
	config.AllowBrowserExtensions = true

	if allowedOrigins := strings.Trim(os.Getenv("OLLAMA_ORIGINS"), "\"'"); allowedOrigins != "" {
		config.AllowOrigins = strings.Split(allowedOrigins, ",")
	}

	for _, allowOrigin := range defaultAllowOrigins {
		config.AllowOrigins = append(config.AllowOrigins,
			fmt.Sprintf("http://%s", allowOrigin),
			fmt.Sprintf("https://%s", allowOrigin),
			fmt.Sprintf("http://%s:*", allowOrigin),
			fmt.Sprintf("https://%s:*", allowOrigin),
		)
	}

	r := gin.Default()
	r.Use(
		cors.New(config),
		allowedHostsMiddleware(s.addr),
	)

	r.GET("/website/*filename", s.WebsiteHandler)
	r.POST("/api/pull", s.PullModelHandler)
	r.POST("/api/generate", s.GenerateHandler)
	r.POST("/api/chat", s.ChatHandler)
	r.POST("/api/embeddings", s.EmbeddingsHandler)
	r.POST("/api/create", s.CreateModelHandler)
	r.POST("/api/push", s.PushModelHandler)
	r.POST("/api/copy", s.CopyModelHandler)
	r.DELETE("/api/delete", s.DeleteModelHandler)
	r.POST("/api/show", s.ShowModelHandler)
	r.POST("/api/blobs/:digest", s.CreateBlobHandler)
	r.HEAD("/api/blobs/:digest", s.HeadBlobHandler)

	// Compatibility endpoints
	r.POST("/v1/chat/completions", openai.Middleware(), s.ChatHandler)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r.Handle(method, "/", func(c *gin.Context) {
			c.String(http.StatusOK, "Ollama is running")
		})

		r.Handle(method, "/api/tags", s.ListModelsHandler)
		r.Handle(method, "/api/version", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"version": version.Version})
		})
	}

	return r
}

func Serve(ln net.Listener) error {
	level := slog.LevelInfo
	if debug := os.Getenv("OLLAMA_DEBUG"); debug != "" {
		level = slog.LevelDebug
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.SourceKey {
				source := attr.Value.Any().(*slog.Source)
				source.File = filepath.Base(source.File)
			}

			return attr
		},
	})

	slog.SetDefault(slog.New(handler))

	blobsDir, err := GetBlobsPath("")
	if err != nil {
		return err
	}
	if err := fixBlobs(blobsDir); err != nil {
		return err
	}

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

	ctx, done := context.WithCancel(context.Background())
	sched := InitScheduler(ctx)
	s := &Server{addr: ln.Addr(), sched: sched}
	r := s.GenerateRoutes()

	slog.Info(fmt.Sprintf("Listening on %s (version %s)", ln.Addr(), version.Version))
	srvr := &http.Server{
		Handler: r,
	}

	// listen for a ctrl+c and stop any loaded llm
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		done()
		sched.unloadAllRunners()
		gpu.Cleanup()
		os.Exit(0)
	}()

	if err := llm.Init(); err != nil {
		return fmt.Errorf("unable to initialize llm library %w", err)
	}

	s.sched.Run(ctx)

	// At startup we retrieve GPU information so we can get log messages before loading a model
	// This will log warnings to the log in case we have problems with detected GPUs
	_ = gpu.GetGPUInfo()

	return srvr.Serve(ln)
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
			slog.Info(fmt.Sprintf("streamResponse: json.Marshal failed with %s", err))
			return false
		}

		// Delineate chunks with new-line delimiter
		bts = append(bts, '\n')
		if _, err := w.Write(bts); err != nil {
			slog.Info(fmt.Sprintf("streamResponse: w.Write failed with %s", err))
			return false
		}

		return true
	})
}

// ChatPrompt builds up a prompt from a series of messages for the currently `loaded` model
func chatPrompt(ctx context.Context, runner *runnerRef, template string, messages []api.Message, numCtx int) (string, error) {
	encode := func(s string) ([]int, error) {
		return runner.llama.Tokenize(ctx, s)
	}

	prompt, err := ChatPrompt(template, messages, numCtx, encode)
	if err != nil {
		return "", err
	}

	return prompt, nil
}

func (s *Server) ChatHandler(c *gin.Context) {
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

	model, err := GetModel(req.Model)
	if err != nil {
		var pErr *fs.PathError
		if errors.As(err, &pErr) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if model.IsEmbedding() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "embedding models do not support chat"})
		return
	}

	opts, err := modelOptions(model, req.Options)
	if err != nil {
		if errors.Is(err, api.ErrInvalidOpts) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var sessionDuration time.Duration
	if req.KeepAlive == nil {
		sessionDuration = getDefaultSessionDuration()
	} else {
		sessionDuration = req.KeepAlive.Duration
	}

	rCh, eCh := s.sched.GetRunner(c.Request.Context(), model, opts, sessionDuration)
	var runner *runnerRef
	select {
	case runner = <-rCh:
	case err = <-eCh:
		handleErrorResponse(c, err)
		return
	}

	checkpointLoaded := time.Now()

	// if the first message is not a system message, then add the model's default system message
	if len(req.Messages) > 0 && req.Messages[0].Role != "system" {
		req.Messages = append([]api.Message{
			{
				Role:    "system",
				Content: model.System,
			},
		}, req.Messages...)
	}

	prompt, err := chatPrompt(c.Request.Context(), runner, model.Template, req.Messages, opts.NumCtx)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// an empty request loads the model
	if len(req.Messages) == 0 || prompt == "" {
		resp := api.ChatResponse{
			CreatedAt: time.Now().UTC(),
			Model:     req.Model,
			Done:      true,
			Message:   api.Message{Role: "assistant"},
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// only send images that are in the prompt
	var i int
	var images []llm.ImageData
	for _, m := range req.Messages {
		for _, img := range m.Images {
			if !isSupportedImageType(img) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "unsupported image format"})
				return
			}

			if strings.Contains(prompt, fmt.Sprintf("[img-%d]", i)) {
				images = append(images, llm.ImageData{Data: img, ID: i})
			}
			i += 1
		}
	}

	slog.Debug("chat handler", "prompt", prompt, "images", len(images))

	ch := make(chan any)

	go func() {
		defer close(ch)

		fn := func(r llm.CompletionResponse) {

			resp := api.ChatResponse{
				Model:     req.Model,
				CreatedAt: time.Now().UTC(),
				Message:   api.Message{Role: "assistant", Content: r.Content},
				Done:      r.Done,
				Metrics: api.Metrics{
					PromptEvalCount:    r.PromptEvalCount,
					PromptEvalDuration: r.PromptEvalDuration,
					EvalCount:          r.EvalCount,
					EvalDuration:       r.EvalDuration,
				},
			}

			if r.Done {
				resp.TotalDuration = time.Since(checkpointStart)
				resp.LoadDuration = checkpointLoaded.Sub(checkpointStart)
			}

			ch <- resp
		}

		if err := runner.llama.Completion(c.Request.Context(), llm.CompletionRequest{
			Prompt:  prompt,
			Format:  req.Format,
			Images:  images,
			Options: opts,
		}, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		// Accumulate responses into the final response
		var final api.ChatResponse
		var sb strings.Builder
		for resp := range ch {
			switch r := resp.(type) {
			case api.ChatResponse:
				sb.WriteString(r.Message.Content)
				final = r
			case gin.H:
				if errorMsg, ok := r["error"].(string); ok {
					c.JSON(http.StatusInternalServerError, gin.H{"error": errorMsg})
					return
				} else {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error format in response"})
					return
				}
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error"})
				return
			}
		}

		final.Message = api.Message{Role: "assistant", Content: sb.String()}
		c.JSON(http.StatusOK, final)
		return
	}

	streamResponse(c, ch)
}

func handleErrorResponse(c *gin.Context, err error) {
	if errors.Is(err, context.Canceled) {
		c.JSON(499, gin.H{"error": "request canceled"})
		return
	}
	if errors.Is(err, ErrMaxQueue) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
