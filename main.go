package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

const (
	uploadDir  = "./uploads"
	maxMemory  = 32 << 20 // 32 MB
	maxFileSize = 10 << 20 // 10 MB por imagen
)

type ImageResponse struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
}

type UploadResponse struct {
	Success bool            `json:"success"`
	Images  []ImageResponse `json:"images"`
	Errors  []string        `json:"errors,omitempty"`
}

func main() {
	// Crear directorio de uploads si no existe
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal("Error creando directorio uploads:", err)
	}

	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)

	// Routes
	r.Post("/upload", uploadHandler)
	r.Get("/image/{userId}/{id}", downloadHandler)
	r.Get("/health", healthHandler)

	port := ":8080"
	log.Printf("🚀 Servidor iniciado en http://localhost%s", port)
	log.Fatal(http.ListenAndServe(port, r))
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	// Parse multipart form
	if err := r.ParseMultipartForm(maxMemory); err != nil {
		respondError(w, http.StatusBadRequest, "Error parseando formulario")
		return
	}

	// Obtener user_id del formulario
	userID := r.FormValue("user_id")
	if userID == "" {
		respondError(w, http.StatusBadRequest, "user_id es requerido")
		return
	}

	// Crear directorio del usuario si no existe
	userDir := filepath.Join(uploadDir, userID)
	if err := os.MkdirAll(userDir, 0755); err != nil {
		respondError(w, http.StatusInternalServerError, "Error creando directorio de usuario")
		return
	}

	files := r.MultipartForm.File["images"]
	if len(files) == 0 {
		respondError(w, http.StatusBadRequest, "No se recibieron imágenes")
		return
	}

	response := UploadResponse{
		Success: true,
		Images:  make([]ImageResponse, 0),
		Errors:  make([]string, 0),
	}

	// Procesar cada imagen
	for _, fileHeader := range files {
		// Validar tamaño
		if fileHeader.Size > maxFileSize {
			response.Errors = append(response.Errors, 
				fmt.Sprintf("%s: excede tamaño máximo de 10MB", fileHeader.Filename))
			continue
		}

		// Validar tipo de archivo
		if !isValidImageType(fileHeader.Filename) {
			response.Errors = append(response.Errors, 
				fmt.Sprintf("%s: formato no válido", fileHeader.Filename))
			continue
		}

		file, err := fileHeader.Open()
		if err != nil {
			response.Errors = append(response.Errors, 
				fmt.Sprintf("%s: error abriendo archivo", fileHeader.Filename))
			continue
		}
		defer file.Close()

		// Generar UUID
		imageID := uuid.New().String()
		
		// Obtener extensión
		ext := filepath.Ext(fileHeader.Filename)
		filename := imageID + ext

		// Guardar imagen
		destPath := filepath.Join(userDir, filename)
		destFile, err := os.Create(destPath)
		if err != nil {
			response.Errors = append(response.Errors, 
				fmt.Sprintf("%s: error guardando", fileHeader.Filename))
			continue
		}
		defer destFile.Close()

		size, err := io.Copy(destFile, file)
		if err != nil {
			os.Remove(destPath) // Limpiar archivo incompleto
			response.Errors = append(response.Errors, 
				fmt.Sprintf("%s: error escribiendo", fileHeader.Filename))
			continue
		}

		// Agregar a respuesta exitosa
		response.Images = append(response.Images, ImageResponse{
			ID:       imageID,
			UserID:   userID,
			Filename: fileHeader.Filename,
			Size:     size,
			URL:      fmt.Sprintf("/image/%s/%s", userID, imageID),
		})

		log.Printf("✓ Imagen guardada: %s/%s (%d bytes)", userID, filename, size)
	}

	// Si todas fallaron
	if len(response.Images) == 0 {
		response.Success = false
		w.WriteHeader(http.StatusBadRequest)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userId")
	imageID := chi.URLParam(r, "id")

	// Buscar archivo con cualquier extensión válida
	validExts := []string{".jpg", ".jpeg", ".png", ".gif", ".webp"}
	var filePath string
	var found bool

	userDir := filepath.Join(uploadDir, userID)
	for _, ext := range validExts {
		path := filepath.Join(userDir, imageID+ext)
		if _, err := os.Stat(path); err == nil {
			filePath = path
			found = true
			break
		}
	}

	if !found {
		http.Error(w, "Imagen no encontrada", http.StatusNotFound)
		return
	}

	// Abrir archivo
	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "Error leyendo imagen", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Obtener info del archivo
	fileInfo, err := file.Stat()
	if err != nil {
		http.Error(w, "Error obteniendo info", http.StatusInternalServerError)
		return
	}

	// Detectar content type
	ext := filepath.Ext(filePath)
	contentType := getContentType(ext)

	// Headers
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))
	w.Header().Set("Cache-Control", "public, max-age=31536000")
	
	// ETag para cache
	etag := generateETag(imageID)
	w.Header().Set("ETag", etag)

	// Check if-none-match
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Servir archivo
	io.Copy(w, file)
	log.Printf("✓ Imagen servida: %s/%s", userID, imageID)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"service": "image-microservice",
	})
}

func isValidImageType(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	validExts := map[string]bool{
		".jpg":  true,
		".jpeg": true,
		".png":  true,
		".gif":  true,
		".webp": true,
	}
	return validExts[ext]
}

func getContentType(ext string) string {
	types := map[string]string{
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".webp": "image/webp",
	}
	if ct, ok := types[ext]; ok {
		return ct
	}
	return "application/octet-stream"
}

func generateETag(id string) string {
	hash := sha256.Sum256([]byte(id))
	return fmt.Sprintf(`"%x"`, hash[:8])
}

func respondError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}
