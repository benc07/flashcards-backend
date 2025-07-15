package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type Card struct {
	ID    string `json:"id"`
	Front string `json:"front"`
	Back  string `json:"back"`
	// DeckID omitted from returning Card in some endpoints; include if useful:
	DeckID string `json:"deckId,omitempty"`
}

type Deck struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	UserID      string `json:"userId"`
	Cards       []Card `json:"cards"`
}

var db *sql.DB

func main() {
	var err error
	db, err = sql.Open("sqlite3", "file:flashcards.db?_foreign_keys=on")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := runMigrations(db); err != nil {
		log.Fatalf("migrations: %v", err)
	}

	if err := runMigrations(db); err != nil {
		log.Fatalf("migrations: %v", err)
	}

	// Ensure initial user with ID "0"
	if err := ensureInitialUser(); err != nil {
		log.Fatalf("failed to insert initial user: %v", err)
	}

	r := chi.NewRouter()
	// Users
	r.Post("/users", createUserHandler)
	r.Get("/users", listUsersHandler)        // ?username=
	r.Get("/users/{userId}", getUserHandler) // single user

	// Decks
	r.Post("/decks", createDeckHandler)            // optionally with cards
	r.Get("/decks", listDecksHandler)              // ?name=
	r.Get("/decks/{deckId}", getDeckHandler)       // single deck
	r.Patch("/decks/{deckId}", patchDeckHandler)   // partial update
	r.Delete("/decks/{deckId}", deleteDeckHandler) // deletes cards via FK cascade

	// Cards
	r.Post("/cards", createCardHandler)          // create card & assign deckId
	r.Patch("/cards/{cardId}", patchCardHandler) // partial update
	r.Delete("/cards/{cardId}", deleteCardHandler)

	fmt.Println("Server listening on :8080")
	http.ListenAndServe(":8080", r)
}

func runMigrations(db *sql.DB) error {
	// Enable foreign keys (in case the DSN flag didn't)
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		return err
	}

	schema := `
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS decks (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    user_id TEXT NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS cards (
    id TEXT PRIMARY KEY,
    deck_id TEXT NOT NULL,
    front TEXT NOT NULL,
    back TEXT NOT NULL,
    FOREIGN KEY (deck_id) REFERENCES decks(id) ON DELETE CASCADE
);
`
	_, err := db.Exec(schema)
	return err
}

func ensureInitialUser() error {
	_, err := db.Exec(`INSERT OR IGNORE INTO users(id, username) VALUES (?, ?)`, "0", "initial_user")
	return err
}

/* ---------- Helpers ---------- */

func respondJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func respondError(w http.ResponseWriter, code int, msg string) {
	respondJSON(w, code, map[string]string{"error": msg})
}

func genID() string {
	return uuid.New().String()
}

/* ---------- Handlers: Users ---------- */

// POST /users
// body: { "username": "..." }
func createUserHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		respondError(w, http.StatusBadRequest, "username required")
		return
	}
	id := genID()
	_, err := db.Exec(`INSERT INTO users(id, username) VALUES (?, ?)`, id, req.Username)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			respondError(w, http.StatusConflict, "username already exists")
			return
		}
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	user := User{ID: id, Username: req.Username}
	respondJSON(w, http.StatusCreated, user)
}

// GET /users?username= (partial match)
func listUsersHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("username")
	var rows *sql.Rows
	var err error
	if q == "" {
		rows, err = db.Query(`SELECT id, username FROM users`)
	} else {
		rows, err = db.Query(`SELECT id, username FROM users WHERE username LIKE ?`, "%"+q+"%")
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username); err != nil {
			respondError(w, http.StatusInternalServerError, "db error")
			return
		}
		out = append(out, u)
	}
	respondJSON(w, http.StatusOK, out)
}

// GET /users/{userId}
func getUserHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "userId")
	var u User
	err := db.QueryRow(`SELECT id, username FROM users WHERE id = ?`, id).Scan(&u.ID, &u.Username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "user not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	respondJSON(w, http.StatusOK, u)
}

/* ---------- Handlers: Decks ---------- */

// POST /decks
// body: { name, description, userId, cards?: [{front,back}, ...] }
func createDeckHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string        `json:"name"`
		Description string        `json:"description"`
		UserID      string        `json:"userId"`
		Cards       []CardRequest `json:"cards"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.UserID) == "" {
		respondError(w, http.StatusBadRequest, "name and userId required")
		return
	}
	// Ensure user exists
	var tmp string
	if err := db.QueryRow(`SELECT id FROM users WHERE id = ?`, req.UserID).Scan(&tmp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusBadRequest, "user does not exist")
			return
		}
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}

	tx, err := db.Begin()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback()

	deckID := genID()
	_, err = tx.Exec(`INSERT INTO decks(id, name, description, user_id) VALUES (?, ?, ?, ?)`, deckID, req.Name, req.Description, req.UserID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	// insert cards if any
	for _, c := range req.Cards {
		cardID := genID()
		if strings.TrimSpace(c.Front) == "" || strings.TrimSpace(c.Back) == "" {
			respondError(w, http.StatusBadRequest, "card front/back required")
			return
		}
		if _, err := tx.Exec(`INSERT INTO cards(id, deck_id, front, back) VALUES (?, ?, ?, ?)`, cardID, deckID, c.Front, c.Back); err != nil {
			respondError(w, http.StatusInternalServerError, "db error")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}

	deck, err := fetchDeckByID(deckID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	respondJSON(w, http.StatusCreated, deck)
}

type CardRequest struct {
	Front string `json:"front"`
	Back  string `json:"back"`
}

// GET /decks?name=  (partial match)
func listDecksHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("name")
	var rows *sql.Rows
	var err error
	if q == "" {
		rows, err = db.Query(`SELECT id FROM decks`)
	} else {
		rows, err = db.Query(`SELECT id FROM decks WHERE name LIKE ?`, "%"+q+"%")
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer rows.Close()

	var decks []Deck
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			respondError(w, http.StatusInternalServerError, "db error")
			return
		}
		d, err := fetchDeckByID(id)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "db error")
			return
		}
		decks = append(decks, d)
	}
	respondJSON(w, http.StatusOK, decks)
}

// GET /decks/{deckId}
func getDeckHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "deckId")
	d, err := fetchDeckByID(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "deck not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	respondJSON(w, http.StatusOK, d)
}

func fetchDeckByID(id string) (Deck, error) {
	var d Deck
	var desc sql.NullString
	err := db.QueryRow(`SELECT id, name, description, user_id FROM decks WHERE id = ?`, id).Scan(&d.ID, &d.Name, &desc, &d.UserID)
	if err != nil {
		return d, err
	}
	if desc.Valid {
		d.Description = desc.String
	}
	// fetch cards
	rows, err := db.Query(`SELECT id, front, back FROM cards WHERE deck_id = ?`, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.Front, &c.Back); err != nil {
			return d, err
		}
		d.Cards = append(d.Cards, c)
	}
	return d, nil
}

// PATCH /decks/{deckId}  (partial)
func patchDeckHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "deckId")
	var patch struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json")
		return
	}
	updates := map[string]interface{}{}
	if patch.Name != nil {
		updates["name"] = *patch.Name
	}
	if patch.Description != nil {
		updates["description"] = *patch.Description
	}
	if len(updates) == 0 {
		respondError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	setParts := []string{}
	args := []interface{}{}
	for k, v := range updates {
		setParts = append(setParts, fmt.Sprintf("%s = ?", k))
		args = append(args, v)
	}
	args = append(args, id)
	query := fmt.Sprintf("UPDATE decks SET %s WHERE id = ?", strings.Join(setParts, ", "))
	res, err := db.Exec(query, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	rowsAff, _ := res.RowsAffected()
	if rowsAff == 0 {
		respondError(w, http.StatusNotFound, "deck not found")
		return
	}
	d, err := fetchDeckByID(id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	respondJSON(w, http.StatusOK, d)
}

// DELETE /decks/{deckId}
func deleteDeckHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "deckId")
	res, err := db.Exec(`DELETE FROM decks WHERE id = ?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		respondError(w, http.StatusNotFound, "deck not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- Handlers: Cards ---------- */

// POST /cards
// body: { deckId, front, back }
func createCardHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeckID string `json:"deckId"`
		Front  string `json:"front"`
		Back   string `json:"back"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.DeckID) == "" || strings.TrimSpace(req.Front) == "" || strings.TrimSpace(req.Back) == "" {
		respondError(w, http.StatusBadRequest, "deckId, front and back required")
		return
	}
	// ensure deck exists
	var tmp string
	if err := db.QueryRow(`SELECT id FROM decks WHERE id = ?`, req.DeckID).Scan(&tmp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusBadRequest, "deck does not exist")
			return
		}
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	id := genID()
	_, err := db.Exec(`INSERT INTO cards(id, deck_id, front, back) VALUES (?, ?, ?, ?)`, id, req.DeckID, req.Front, req.Back)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	card := Card{ID: id, Front: req.Front, Back: req.Back, DeckID: req.DeckID}
	respondJSON(w, http.StatusCreated, card)
}

// PATCH /cards/{cardId}
func patchCardHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cardId")
	var patch struct {
		Front *string `json:"front"`
		Back  *string `json:"back"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json")
		return
	}
	updates := map[string]interface{}{}
	if patch.Front != nil {
		updates["front"] = *patch.Front
	}
	if patch.Back != nil {
		updates["back"] = *patch.Back
	}
	if len(updates) == 0 {
		respondError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	setParts := []string{}
	args := []interface{}{}
	for k, v := range updates {
		setParts = append(setParts, fmt.Sprintf("%s = ?", k))
		args = append(args, v)
	}
	args = append(args, id)
	query := fmt.Sprintf("UPDATE cards SET %s WHERE id = ?", strings.Join(setParts, ", "))
	res, err := db.Exec(query, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	rowsAff, _ := res.RowsAffected()
	if rowsAff == 0 {
		respondError(w, http.StatusNotFound, "card not found")
		return
	}
	// return updated card
	var c Card
	err = db.QueryRow(`SELECT id, front, back, deck_id FROM cards WHERE id = ?`, id).Scan(&c.ID, &c.Front, &c.Back, &c.DeckID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	respondJSON(w, http.StatusOK, c)
}

// DELETE /cards/{cardId}
func deleteCardHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cardId")
	res, err := db.Exec(`DELETE FROM cards WHERE id = ?`, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db error")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		respondError(w, http.StatusNotFound, "card not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
