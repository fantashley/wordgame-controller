package scrabbleserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

type scrabbleServer struct {
	activeGames map[uuid.UUID]*ScrabbleGame
}

// GeneralGameRequest is the catch-all request format for client requests that
// don't require special fields
type GeneralGameRequest struct {
	GameID     uuid.UUID  `json:"game_id"`
	PlayerID   *uuid.UUID `json:"player_id,omitempty"`
	PlayerName *string    `json:"player_name,omitempty"`
}

// GameStateResponse is the format of the response sent to clients when they
// request the current game state
type GameStateResponse struct {
	GameID      uuid.UUID     `json:"game_id"`
	PlayerID    uuid.UUID     `json:"-"`
	Players     []*Player     `json:"players"`
	Board       ScrabbleBoard `json:"board"`
	PlayerTurn  int           `json:"turn"`
	PlayerTiles []byte        `json:"tiles"`
}

// GamePlayRequest is the format of the request a client sends when they would
// like to play their turn
type GamePlayRequest struct {
	GameID   uuid.UUID        `json:"game_id"`
	PlayerID uuid.UUID        `json:"player_id"`
	StartPos SquareCoordinate `json:"start_pos"`
	EndPos   SquareCoordinate `json:"end_pos"`
	Tiles    []byte           `json:"tiles"`
	Swap     bool             `json:"swap"`
	Play     bool             `json:"-"`
}

var (
	serverMu sync.Mutex
	server   scrabbleServer
)

// StartScrabbleServer is the function that is run to start the Scrabble HTTP
// server
func StartScrabbleServer(bindAddr string) error {
	server.activeGames = make(map[uuid.UUID]*ScrabbleGame)

	r := mux.NewRouter()
	r.HandleFunc("/game/create", createGameHandler)
	r.HandleFunc("/game/join", joinGameHandler)
	r.HandleFunc("/game/start", startGameHandler)
	r.HandleFunc("/game/state", gameStateHandler)

	return http.ListenAndServe(bindAddr, r)
}

// createGameHandler handles API requests for creating a new Scrabble game
// instance
func createGameHandler(w http.ResponseWriter, r *http.Request) {
	newGame := createScrabbleGame()

	resp := GeneralGameRequest{
		GameID: newGame.ID,
	}

	serverMu.Lock()
	server.activeGames[newGame.ID] = newGame
	serverMu.Unlock()

	gameData, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusCreated)
	w.Write(gameData)
}

// joinGameHandler handles requests from players to join a specified game. It
// also creates a player and returns their ID to the client.
func joinGameHandler(w http.ResponseWriter, r *http.Request) {
	var j GeneralGameRequest
	var g *ScrabbleGame

	err := json.NewDecoder(r.Body).Decode(&j)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create player to be added to game
	p := Player{
		ID:    uuid.New(),
		Name:  *j.PlayerName,
		Tiles: make([]byte, 0),
		State: make(chan GameStateResponse),
	}

	// Set field in response so player knows their ID
	j.PlayerID = &p.ID

	// Retrieve the game that matches ID requested
	g, err = getGame(j.GameID, &w)
	if err != nil {
		return
	}

	g.Lock()
	playerCount := len(g.Players)
	// Check that game is valid to join
	if playerCount == 4 {
		g.Unlock()
		http.Error(w, "Maximum players reached for game", http.StatusBadRequest)
		return
	} else if g.Active {
		g.Unlock()
		http.Error(w, "Game has already started", http.StatusBadRequest)
		return
	}
	// Assign player their number based on when they joined
	p.Number = playerCount
	// Add player to game
	g.Players[p.ID] = &p
	g.Unlock()

	// Create response containing game ID and new player ID
	resp, err := json.Marshal(j)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

// startGameHandler is a handler that will start a game upon request, marking it
// as active and no longer joinable by other players. It also kicks off the
// goroutine for the specified game.
func startGameHandler(w http.ResponseWriter, r *http.Request) {
	var j GeneralGameRequest
	var g *ScrabbleGame

	// Decode Game ID
	err := json.NewDecoder(r.Body).Decode(&j)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Retrieve game instance
	g, err = getGame(j.GameID, &w)
	if err != nil {
		return
	}

	// Set game to active
	g.Lock()
	defer g.Unlock()
	// Ensure game is eligible to be started
	if g.Active {
		http.Error(w, "Game has already started", http.StatusBadRequest)
		return
	} else if len(g.Players) < 2 {
		http.Error(w, "At least two players needed to start game", http.StatusBadRequest)
		return
	}
	g.Active = true

	// Start game controller goroutine
	go g.stateController()

	w.WriteHeader(http.StatusOK)
}

// gameStateHandler handles requests for the game's current state. It will
// respond using the GameStateResponse struct.
func gameStateHandler(w http.ResponseWriter, r *http.Request) {
	var j GeneralGameRequest

	// Decode game ID and player ID. Player ID is needed so the server knows
	// which tiles to send for the player's current state.
	err := json.NewDecoder(r.Body).Decode(&j)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Retrieve requested game
	g, err := getGame(j.GameID, &w)
	if err != nil {
		return
	}

	// Send request to game controller
	g.Action <- GamePlayRequest{
		GameID:   j.GameID,
		PlayerID: *j.PlayerID,
	}

	// Decode response with current game state details from game controller
	resp, err := json.Marshal(<-g.Players[*j.PlayerID].State)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

// getGame is a concurrency-safe function that retrieves the requested game
// instance from the list of active games on the server
func getGame(gameID uuid.UUID, w *(http.ResponseWriter)) (*ScrabbleGame, error) {
	var g *ScrabbleGame
	var ok bool
	serverMu.Lock()
	defer serverMu.Unlock()
	if g, ok = server.activeGames[gameID]; !ok {
		http.Error(*w, "No existing game with that ID", http.StatusBadRequest)
		return nil, errors.New("Game does not exist")
	}
	return g, nil
}