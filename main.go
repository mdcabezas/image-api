package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

const (
	uploadDir   = "./uploads"
	maxMemory   = 32 << 20 // 32 MB
	maxFileSize = 10 << 20 // 10 MB por imagen
)

var db *sql.DB

type Image struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Filename  string     `json:"filename"`
	FilePath  string     `json:"file_path"`
	MimeType  string     `json:"mime_type"`
	SizeBytes int64      `json:"size_bytes"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	URL       string     `json:"url"`
}

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

type ListResponse struct {
	UserID string  `json:"user_id"`
	Total  int     `json:"total"`
	Images []Image `json:"images"`
}

func main() {
	// Conectar a MySQL
	var err error
	dsn := os.Getenv("MYSQL_DSN_IMAGE")
	if dsn == "" {
		dsn = "root:password@tcp(localhost:3306)/images_db?parseTime=true"
		log.Println("⚠️  MYSQL_DSN_IMAGE no configurado, usando default:", dsn)
	}

	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal("Error conectando a MySQL:", err)
	}
	defer db.Close()

	// Verificar conexión
	if err = db.Ping(); err != nil {
		log.Fatal("Error ping a MySQL:", err)
	}
	log.Println("✅ Conectado a MySQL")

	// Crear tabla si no existe
	if err := createTable(); err != nil {
		log.Fatal("Error creando tabla:", err)
	}

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
	r.Get("/images/{userId}", listImagesHandler)
	r.Delete("/image/{userId}/{id}", deleteImageHandler)
	r.Get("/health", healthHandler)

	port := ":8080"
	log.Printf("🚀 Servidor iniciado en http://localhost%s", port)
	log.Fatal(http.ListenAndServe(port, r))
}

func createTable() error {
	query := `
	CREATE TABLE IF NOT EXISTS images (
		id VARCHAR(36) PRIMARY KEY,
		user_id VARCHAR(100) NOT NULL,
		filename VARCHAR(255) NOT NULL,
		file_path VARCHAR(500) NOT NULL,
		mime_type VARCHAR(50) NOT NULL,
		size_bytes BIGINT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		INDEX idx_user_id (user_id),
		INDEX idx_created_at (created_at),
		INDEX idx_deleted_at (deleted_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`
	_, err := db.Exec(query)
	if err != nil {
		return err
	}
	log.Println("✅ Tabla 'images' verificada/creada")
	return nil
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
		mimeType := getContentType(ext)

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

		// Guardar en BD
		query := `INSERT INTO images (id, user_id, filename, file_path, mime_type, size_bytes) 
				  VALUES (?, ?, ?, ?, ?, ?)`
		_, err = db.Exec(query, imageID, userID, fileHeader.Filename, destPath, mimeType, size)
		if err != nil {
			os.Remove(destPath) // Limpiar archivo si falla BD
			response.Errors = append(response.Errors,
				fmt.Sprintf("%s: error guardando en BD", fileHeader.Filename))
			log.Printf("Error BD: %v", err)
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

	// Buscar en BD
	var img Image
	query := `SELECT id, user_id, filename, file_path, mime_type, size_bytes, created_at, deleted_at 
			  FROM images WHERE id = ? AND user_id = ? AND deleted_at IS NULL`
	err := db.QueryRow(query, imageID, userID).Scan(
		&img.ID, &img.UserID, &img.Filename, &img.FilePath,
		&img.MimeType, &img.SizeBytes, &img.CreatedAt, &img.DeletedAt,
	)

	if err == sql.ErrNoRows {
		http.Error(w, "Imagen no encontrada", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Error BD: %v", err)
		http.Error(w, "Error interno", http.StatusInternalServerError)
		return
	}

	// Abrir archivo
	file, err := os.Open(img.FilePath)
	if err != nil {
		log.Printf("Error abriendo archivo: %v", err)
		http.Error(w, "Error leyendo imagen", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Headers
	w.Header().Set("Content-Type", img.MimeType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", img.SizeBytes))
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

func listImagesHandler(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userId")

	query := `SELECT id, user_id, filename, file_path, mime_type, size_bytes, created_at 
			  FROM images WHERE user_id = ? AND deleted_at IS NULL ORDER BY created_at DESC`

	rows, err := db.Query(query, userID)
	if err != nil {
		log.Printf("Error BD: %v", err)
		respondError(w, http.StatusInternalServerError, "Error consultando BD")
		return
	}
	defer rows.Close()

	images := make([]Image, 0)
	for rows.Next() {
		var img Image
		err := rows.Scan(&img.ID, &img.UserID, &img.Filename, &img.FilePath,
			&img.MimeType, &img.SizeBytes, &img.CreatedAt)
		if err != nil {
			log.Printf("Error escaneando fila: %v", err)
			continue
		}
		img.URL = fmt.Sprintf("/image/%s/%s", img.UserID, img.ID)
		images = append(images, img)
	}

	response := ListResponse{
		UserID: userID,
		Total:  len(images),
		Images: images,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func deleteImageHandler(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userId")
	imageID := chi.URLParam(r, "id")

	// Soft delete
	query := `UPDATE images SET deleted_at = NOW() WHERE id = ? AND user_id = ? AND deleted_at IS NULL`
	result, err := db.Exec(query, imageID, userID)
	if err != nil {
		log.Printf("Error BD: %v", err)
		respondError(w, http.StatusInternalServerError, "Error eliminando imagen")
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "Imagen no encontrada")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Imagen eliminada",
		"id":      imageID,
	})
	log.Printf("✓ Imagen eliminada (soft): %s/%s", userID, imageID)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	// Check BD
	err := db.Ping()
	status := "ok"
	if err != nil {
		status = "degraded"
		log.Printf("Health check: BD no disponible - %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  status,
		"service": "image-microservice",
		"db":      status,
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
