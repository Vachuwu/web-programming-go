package main

import (
	"context"
	"log"
	"net/http"

	"playlist/internal/config"
	"playlist/internal/database"
	"playlist/internal/handler"
	"playlist/internal/repository"
	"playlist/internal/service"
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	pool, err := database.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	users := service.NewUserService(repository.NewUserRepository(pool))
	playlists := service.NewPlaylistService(repository.NewPlaylistRepository(pool))
	httpHandler := handler.New(users, playlists)

	log.Printf("playlist server listening on %s", cfg.HTTPAddress)
	if err := http.ListenAndServe(cfg.HTTPAddress, httpHandler.Routes()); err != nil {
		log.Fatal(err)
	}
}
