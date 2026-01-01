package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Could not retrieve video data", err)
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You do not have access to that video", err)
	}

	const uploadLimit = 10 << 30
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

	f, fHdr, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer f.Close()

	mediaType := fHdr.Header.Get("Content-Type")

	_, _, err = mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse video", err)
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	fmt.Println(tempFile.Name())
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	io.Copy(tempFile, f)

	tempFile.Seek(0, io.SeekStart)

	ar, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to determine aspect ratio", err)
		return
	}

	var prefix string
	switch ar {
	case "16:9":
		prefix = "landscape/"

	case "9:16":
		prefix = "portrait/"

	case "other":
	default:
		prefix = "other/"
	}

	var buf = make([]byte, 32)
	rand.Read(buf)
	videoKey := prefix + base64.RawURLEncoding.EncodeToString((buf)) + ".mp4"

	// process the video
	processedFile, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not process video", err)
		return
	}
	defer os.Remove(processedFile)

	f2, err := os.Open(processedFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not open processed video file", err)
		return
	}
	defer f2.Close()

	cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &videoKey,
		Body:        f2,
		ContentType: &mediaType,
	})

	videoUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, videoKey)
	video.VideoURL = &videoUrl

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	out := bytes.Buffer{}
	cmd.Stdout = &out

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var video struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	err = json.Unmarshal(out.Bytes(), &video)
	if err != nil {
		return "", err
	}

	h := video.Streams[0].Height
	w := video.Streams[0].Width

	if h*16/9 == w ||
		w*9/16 == h {
		return "16:9", nil
	}

	if h*9/16 == w ||
		w*16/9 == h {
		return "9:16", nil
	}

	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outFilePath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outFilePath)
	err := cmd.Run()

	if err != nil {
		return "", err
	}

	return outFilePath, nil
}