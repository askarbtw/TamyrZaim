package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/mattn/go-sqlite3"
)

// Conversation holds details for multi-step interactions.
type Conversation struct {
	Operation string            // "addloan" or "repay"
	Step      int               // Step number in the conversation
	Data      map[string]string // Temporary data storage
}

// Global conversation state map (keyed by chat ID)
var convMutex sync.Mutex
var conversations = make(map[int64]*Conversation)

// Global bot variable
var bot *tgbotapi.BotAPI

const BOT_TOKEN string = "TamyrZaimbot"

func main() {
	// Get BOT_TOKEN from environment variable.
	_ = os.Setenv(BOT_TOKEN, "7694442474:AAHrnMXRFyaf3jzLmMBBO0wGiZdK873dxvM")
	botToken := os.Getenv(BOT_TOKEN)
	if botToken == "" {
		log.Fatal("BOT_TOKEN environment variable not set.")
	}

	var err error
	bot, err = tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("Error creating bot: %v", err)
	}
	log.Printf("Authorized on account %s", bot.Self.UserName)

	// Open SQLite database.
	db, err := sql.Open("sqlite3", "./lending.db")
	if err != nil {
		log.Fatalf("Error opening DB: %v", err)
	}
	defer db.Close()

	// Create loans table using the updated schema.
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS loans (
    user_id INTEGER NOT NULL,
    loan_id INTEGER NOT NULL,
    borrower_name TEXT NOT NULL,
    amount REAL NOT NULL,
    purpose TEXT,
    repaid BOOLEAN DEFAULT 0,
    PRIMARY KEY (user_id, loan_id)
);`
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatalf("Error creating table: %v", err)
	}

	// Start weekly reminder scheduler.
	go reminderScheduler(bot, db)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			handleCallbackQuery(bot, db, update.CallbackQuery)
			continue
		}

		if update.Message == nil || update.Message.Text == "" {
			continue
		}

		handleMessage(bot, db, update.Message)
	}
}

// handleMessage processes incoming messages.
func handleMessage(bot *tgbotapi.BotAPI, db *sql.DB, message *tgbotapi.Message) {
	chatID := message.Chat.ID
	text := strings.TrimSpace(message.Text)

	if message.IsCommand() {
		switch message.Command() {
		case "start":
			sendMainMenu(bot, chatID)
			return
		default:
			sendMessage(bot, chatID, "Unknown command. Please use /start.")
			return
		}
	}

	handleConversationInput(bot, db, chatID, text)
}

// handleConversationInput routes user input to the correct conversation handler.
func handleConversationInput(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, text string) {
	convMutex.Lock()
	conv, exists := conversations[chatID]
	convMutex.Unlock()

	if !exists {
		sendMainMenu(bot, chatID)
		return
	}

	switch conv.Operation {
	case "addloan":
		handleAddLoanStep(bot, db, chatID, text, conv)
	case "repay":
		handleRepayStep(bot, db, chatID, text, conv)
	default:
		sendMainMenu(bot, chatID)
	}
}

// handleAddLoanStep processes each step of adding a loan.
func handleAddLoanStep(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, text string, conv *Conversation) {
	switch conv.Step {
	case 0:
		// Ask for borrower's name.
		if text == "" {
			sendMessage(bot, chatID, "Borrower's name cannot be empty. Please enter a valid name:")
			return
		}
		conv.Data["borrower_name"] = text
		conv.Step++
		sendMessage(bot, chatID, "Enter loan amount (e.g., 100.50):")
	case 1:
		// Validate and save loan amount.
		amount, err := strconv.ParseFloat(text, 64)
		if err != nil {
			sendMessage(bot, chatID, "Invalid amount. Please enter a valid number (e.g., 100.50):")
			return
		}
		conv.Data["amount"] = fmt.Sprintf("%.2f", amount)
		conv.Step++
		sendMessage(bot, chatID, "Enter the purpose of the loan:")
	case 2:
		// Save purpose and complete recording process.
		if text == "" {
			sendMessage(bot, chatID, "Purpose cannot be empty. Please enter a valid purpose:")
			return
		}
		conv.Data["purpose"] = text

		var newLoanID int
		err := db.QueryRow("SELECT COALESCE(MAX(loan_id), 0) + 1 FROM loans WHERE user_id = ?", chatID).Scan(&newLoanID)
		if err != nil {
			sendMessage(bot, chatID, fmt.Sprintf("Error generating loan ID: %v", err))
			return
		}

		// Insert the loan into the database.
		query := `INSERT INTO loans (user_id, loan_id, borrower_name, amount, purpose, repaid) VALUES (?, ?, ?, ?, ?, 0)`
		_, err = db.Exec(query, chatID, newLoanID, conv.Data["borrower_name"], conv.Data["amount"], conv.Data["purpose"])
		if err != nil {
			sendMessage(bot, chatID, fmt.Sprintf("Failed to record loan: %v", err))
			return
		}

		sendMessage(bot, chatID, fmt.Sprintf(
			"Loan successfully recorded!\n\nBorrower: %s\nAmount: %sKZT\nPurpose: %s",
			conv.Data["borrower_name"],
			conv.Data["amount"],
			conv.Data["purpose"],
		))
		endConversation(chatID)
		sendMainMenu(bot, chatID)
	default:
		sendMessage(bot, chatID, "An error occurred in the add loan process. Please try again.")
		endConversation(chatID)
	}
}

// handleRepayStep processes each step of recording a repayment.
func handleRepayStep(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, text string, conv *Conversation) {
	switch conv.Step {
	case 0:
		// Ask for Loan ID to mark as repaid.
		if text == "" {
			sendMessage(bot, chatID, "Please enter a valid Loan ID:")
			return
		}
		conv.Data["loan_id"] = text
		conv.Step++
		id, err := strconv.Atoi(text)
		if err != nil {
			sendMessage(bot, chatID, "Invalid Loan ID. Please enter a valid number:")
			return
		}
		// Check if loan exists and belongs to user.
		var exists bool
		row := db.QueryRow("SELECT EXISTS(SELECT 1 FROM loans WHERE loan_id = ? AND user_id = ? AND repaid = 0)", id, chatID)
		err = row.Scan(&exists)
		if err != nil || !exists {
			sendMessage(bot, chatID, "Loan not found or already repaid. Please try again with a valid Loan ID.")
			endConversation(chatID)
			return
		}
		// Mark the loan as repaid.
		_, err = db.Exec("UPDATE loans SET repaid = 1 WHERE loan_id = ? AND user_id = ?", id, chatID)
		if err != nil {
			sendMessage(bot, chatID, fmt.Sprintf("Failed to mark loan as repaid: %v", err))
			return
		}
		sendMessage(bot, chatID, fmt.Sprintf("Loan ID %d has been marked as repaid. 🎉", id))
		endConversation(chatID)
		sendMainMenu(bot, chatID)
	default:
		sendMessage(bot, chatID, "An error occurred in the repayment process. Please try again.")
		endConversation(chatID)
	}
}

// endConversation clears the conversation state for a user.
func endConversation(chatID int64) {
	convMutex.Lock()
	delete(conversations, chatID)
	convMutex.Unlock()
}

// sendMainMenu displays the main inline keyboard menu.
func sendMainMenu(bot *tgbotapi.BotAPI, chatID int64) {
	menuButtons := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Record Loan", "menu_addloan"),
			tgbotapi.NewInlineKeyboardButtonData("Record Repayment", "menu_repay"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Balance", "menu_balance"),
			tgbotapi.NewInlineKeyboardButtonData("Stats", "menu_stats"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "Please choose an option:")
	msg.ReplyMarkup = menuButtons
	bot.Send(msg)
}

// sendMessage is a helper function to send messages.
func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	bot.Send(msg)
}

// handleCallbackQuery processes inline keyboard callback queries.
func handleCallbackQuery(bot *tgbotapi.BotAPI, db *sql.DB, cq *tgbotapi.CallbackQuery) {
	data := cq.Data
	chatID := cq.Message.Chat.ID

	switch data {
	case "menu_addloan":
		startAddLoanConversation(chatID)
		bot.Request(tgbotapi.NewCallback(cq.ID, "Starting loan recording..."))
	case "menu_repay":
		startRepayConversation(chatID)
		bot.Request(tgbotapi.NewCallback(cq.ID, "Starting repayment recording..."))
	case "menu_balance":
		showBalance(bot, db, chatID)
		bot.Request(tgbotapi.NewCallback(cq.ID, "Fetching your balance..."))
	case "menu_stats":
		showStats(bot, db, chatID)
		bot.Request(tgbotapi.NewCallback(cq.ID, "Fetching your stats..."))
	default:
		sendMessage(bot, chatID, "Unknown option. Please try again.")
		bot.Request(tgbotapi.NewCallback(cq.ID, "Unknown option selected."))
	}
}

// startAddLoanConversation initializes a conversation for adding a loan.
func startAddLoanConversation(chatID int64) {
	convMutex.Lock()
	conversations[chatID] = &Conversation{
		Operation: "addloan",
		Step:      0,
		Data:      make(map[string]string),
	}
	convMutex.Unlock()
	sendMessage(bot, chatID, "Let's record a new loan.\nEnter borrower's name:")
}

// startRepayConversation initializes a conversation for recording a repayment.
func startRepayConversation(chatID int64) {
	convMutex.Lock()
	conversations[chatID] = &Conversation{
		Operation: "repay",
		Step:      0,
		Data:      make(map[string]string),
	}
	convMutex.Unlock()
	sendMessage(bot, chatID, "Let's record a repayment.\nEnter the Loan ID to mark as repaid:")
}

// showBalance retrieves and displays the user's active loans.
func showBalance(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64) {
	rows, err := db.Query("SELECT loan_id, borrower_name, amount FROM loans WHERE user_id = ? AND repaid = 0", chatID)
	if err != nil {
		sendMessage(bot, chatID, fmt.Sprintf("Error retrieving balance: %v", err))
		return
	}
	defer rows.Close()

	var response strings.Builder
	response.WriteString("📊 Active Loans:\n")
	var totalAmount float64

	for rows.Next() {
		var id int
		var borrower string
		var amount float64
		err := rows.Scan(&id, &borrower, &amount)
		if err != nil {
			continue
		}
		totalAmount += amount
		response.WriteString(fmt.Sprintf("💳 Loan ID: %d\n👤 Borrower: %s\n💰 Amount: %.2fKZT\n\n", id, borrower, amount))
	}

	response.WriteString(fmt.Sprintf("💼 Total Active Loan Amount: %.2fKZT", totalAmount))
	sendMessage(bot, chatID, response.String())
}

// showStats generates and displays lending statistics.
func showStats(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64) {
	var totalLoans int
	var totalLent float64
	var totalRepaid int

	err := db.QueryRow("SELECT COUNT(*), SUM(amount) FROM loans WHERE user_id = ?", chatID).Scan(&totalLoans, &totalLent)
	if err != nil {
		sendMessage(bot, chatID, fmt.Sprintf("Error generating stats: %v", err))
		return
	}

	err = db.QueryRow("SELECT COUNT(*) FROM loans WHERE user_id = ? AND repaid = 1", chatID).Scan(&totalRepaid)
	if err != nil {
		sendMessage(bot, chatID, fmt.Sprintf("Error generating stats: %v", err))
		return
	}

	stats := fmt.Sprintf(
		"📈 Lending Stats:\n\n"+
			"🔢 Total Loans: %d\n"+
			"💰 Total Lent: %.2fKZT\n"+
			"✅ Loans Repaid: %d\n"+
			"📊 Loans Pending: %d",
		totalLoans,
		totalLent,
		totalRepaid,
		totalLoans-totalRepaid,
	)
	sendMessage(bot, chatID, stats)
}

// reminderScheduler sends weekly reminders to users with pending loans.
func reminderScheduler(bot *tgbotapi.BotAPI, db *sql.DB) {
	ticker := time.NewTicker(7 * 24 * time.Hour)
	for {
		<-ticker.C

		// Get distinct user IDs with pending loans.
		rows, err := db.Query("SELECT DISTINCT user_id FROM loans WHERE repaid = 0")
		if err != nil {
			log.Printf("Error querying users for reminders: %v", err)
			continue
		}
		var userIDs []int64
		for rows.Next() {
			var userID int64
			if err := rows.Scan(&userID); err != nil {
				continue
			}
			userIDs = append(userIDs, userID)
		}
		rows.Close()

		// For each user, compile a reminder message and send it.
		for _, userID := range userIDs {
			reminderMsg := "⏰ Weekly Reminder: You have pending loans:\n"
			loanRows, err := db.Query("SELECT loan_id, borrower_name, amount FROM loans WHERE user_id = ? AND repaid = 0", userID)
			if err != nil {
				continue
			}
			for loanRows.Next() {
				var id int
				var borrower string
				var amount float64
				if err := loanRows.Scan(&id, &borrower, &amount); err != nil {
					continue
				}
				reminderMsg += fmt.Sprintf("Loan ID %d - %s: %.2fKZT\n", id, borrower, amount)
			}
			loanRows.Close()
			sendMessage(bot, userID, reminderMsg)
		}
	}
}
