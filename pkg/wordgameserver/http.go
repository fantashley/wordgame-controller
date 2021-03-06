package wordgameserver

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
	Error       error         `json:"-"`
}

// GamePlayRequest is the format of the request a client sends when they would
// like to play their turn
type GamePlayRequest struct {
	GameID   uuid.UUID        `json:"game_id"`
	PlayerID uuid.UUID        `json:"player_id"`
	StartPos SquareCoordinate `json:"start_pos"`
	EndPos   SquareCoordinate `json:"end_pos"`
	Tiles    []byte           `json:"tiles"`
	Blanks   []byte           `json:"blanks,omitempty"`
	Swap     bool             `json:"swap"`
	Play     bool             `json:"-"`
}

var (
	serverMu sync.Mutex
	server   = scrabbleServer{
		activeGames: make(map[uuid.UUID]*ScrabbleGame),
	}
)

// StartWordGameServer is the function that is run to start the Word Game HTTP
// server
func StartWordGameServer(bindAddr string) error {
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

	// Decode Game ID
	err := json.NewDecoder(r.Body).Decode(&j)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Retrieve the game that matches ID requested
	g, err = getGame(j.GameID, w)
	if err != nil {
		return
	}

	g.Lock()
	defer g.Unlock()

	// Set field in response so player knows their ID
	playerID, err := g.addPlayer(*j.PlayerName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	j.PlayerID = &playerID

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
	g, err = getGame(j.GameID, w)
	if err != nil {
		return
	}

	// Start game
	g.Lock()
	defer g.Unlock()
	err = g.start()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
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

	// Send request to game controller
	gameRequestHelper(GamePlayRequest{
		GameID:   j.GameID,
		PlayerID: *j.PlayerID,
	}, w)
}

// gamePlayHandler handles requests from players to play a word. It will respond
// using the GameStateResponse struct.
func gamePlayHandler(w http.ResponseWriter, r *http.Request) {
	var j GamePlayRequest

	err := json.NewDecoder(r.Body).Decode(&j)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gameRequestHelper(j, w)
}

// gameRequestHelper relays play and state requests to the game, since they are
// the exact same flow
func gameRequestHelper(j GamePlayRequest, w http.ResponseWriter) {
	// Get game to send message to
	g, err := getGame(j.GameID, w)
	if err != nil {
		return
	}

	// Send state or play request and wait for response
	state, err := g.request(j)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	// Return GameStateResponse as json
	resp, err := json.Marshal(state)
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
func getGame(gameID uuid.UUID, w http.ResponseWriter) (*ScrabbleGame, error) {
	var g *ScrabbleGame
	var ok bool
	serverMu.Lock()
	defer serverMu.Unlock()
	if g, ok = server.activeGames[gameID]; !ok {
		http.Error(w, "No existing game with that ID", http.StatusBadRequest)
		return nil, errors.New("Game does not exist")
	}
	return g, nil
}
