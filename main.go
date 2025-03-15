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
	_ "modernc.org/sqlite"
)

// Constants for state management
const (
	// Operation types
	OpAddLoan      = "addloan"
	OpRepayLoan    = "repayloan"
	OpEditLoan     = "editloan"
	OpDeleteLoan   = "deleteloan"
	OpPartialRepay = "partialrepay"
	OpSearchLoan   = "searchloan"
	OpNone         = ""

	// Menu callback data
	MenuAddLoan = "menu_addloan"
	MenuRepay   = "menu_repay"
	MenuBalance = "menu_balance"
	MenuStats   = "menu_stats"
	MenuManage  = "menu_manage"
	MenuSearch  = "menu_search"

	// Sub-menu callback data
	SubMenuEdit       = "submenu_edit"
	SubMenuDelete     = "submenu_delete"
	SubMenuPartial    = "submenu_partial"
	SubMenuRepayments = "submenu_repayments"

	// Search sub-menu callback data
	SearchByName   = "search_by_name"
	SearchByStatus = "search_by_status"
	SearchAll      = "search_all"
)

// UserState manages the state for a single user
type UserState struct {
	Operation   string
	Step        int
	Data        map[string]string
	LastUpdated time.Time
}

// State manager for all users
type BotManager struct {
	bot             *tgbotapi.BotAPI
	db              *sql.DB
	userStates      map[int64]*UserState
	stateMutex      sync.RWMutex
	lastProcessedID int
}

// Initialize a new bot manager
func NewBotManager(bot *tgbotapi.BotAPI, db *sql.DB) *BotManager {
	return &BotManager{
		bot:        bot,
		db:         db,
		userStates: make(map[int64]*UserState),
	}
}

// GetState returns the current state for a user, creating one if it doesn't exist
func (m *BotManager) GetState(chatID int64) *UserState {
	m.stateMutex.RLock()
	state, exists := m.userStates[chatID]
	m.stateMutex.RUnlock()

	if !exists {
		state = &UserState{
			Operation:   OpNone,
			Step:        0,
			Data:        make(map[string]string),
			LastUpdated: time.Now(),
		}
		m.stateMutex.Lock()
		m.userStates[chatID] = state
		m.stateMutex.Unlock()
	}

	return state
}

// SetState updates a user's state
func (m *BotManager) SetState(chatID int64, operation string, step int) {
	m.stateMutex.Lock()
	defer m.stateMutex.Unlock()

	state, exists := m.userStates[chatID]
	if !exists {
		state = &UserState{
			Data: make(map[string]string),
		}
		m.userStates[chatID] = state
	}

	state.Operation = operation
	state.Step = step
	state.LastUpdated = time.Now()
}

// ClearState resets a user's state
func (m *BotManager) ClearState(chatID int64) {
	m.stateMutex.Lock()
	defer m.stateMutex.Unlock()

	delete(m.userStates, chatID)
}

// SaveStateData stores data in the user state
func (m *BotManager) SaveStateData(chatID int64, key string, value string) {
	m.stateMutex.Lock()
	defer m.stateMutex.Unlock()

	state, exists := m.userStates[chatID]
	if !exists {
		state = &UserState{
			Operation: OpNone,
			Step:      0,
			Data:      make(map[string]string),
		}
		m.userStates[chatID] = state
	}

	if state.Data == nil {
		state.Data = make(map[string]string)
	}

	state.Data[key] = value
}

// SendMessage is a helper to send text messages
func (m *BotManager) SendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := m.bot.Send(msg)
	if err != nil {
		log.Printf("Error sending message: %v", err)
	}
}

// ShowMainMenu displays the main menu keyboard
func (m *BotManager) ShowMainMenu(chatID int64) {
	menuButtons := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💰 Записать займ", MenuAddLoan),
			tgbotapi.NewInlineKeyboardButtonData("✅ Записать возврат", MenuRepay),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Баланс", MenuBalance),
			tgbotapi.NewInlineKeyboardButtonData("📈 Статистика", MenuStats),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Управление займами", MenuManage),
			tgbotapi.NewInlineKeyboardButtonData("🔍 Поиск", MenuSearch),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "🤖 Выберите действие:")
	msg.ReplyMarkup = menuButtons
	_, err := m.bot.Send(msg)
	if err != nil {
		log.Printf("Error showing main menu: %v", err)
	}
}

// StartAddLoanFlow begins the process of recording a new loan
func (m *BotManager) StartAddLoanFlow(chatID int64) {
	// First clear any existing state
	m.ClearState(chatID)

	// Send the initial prompt
	m.SendMessage(chatID, "📝 Давайте запишем новый займ.\n👤 Введите имя заемщика:")

	// Then set the new state
	m.SetState(chatID, OpAddLoan, 0)

	log.Printf("Started add loan flow for user %d", chatID)
}

// StartRepayLoanFlow begins the process of marking a loan as repaid
func (m *BotManager) StartRepayLoanFlow(chatID int64) {
	// First clear any existing state
	m.ClearState(chatID)

	// Get active loans
	activeLoans, err := m.GetActiveLoansForUser(chatID)
	if err != nil {
		log.Printf("Error getting active loans: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить список активных займов.")
		m.ShowMainMenu(chatID)
		return
	}

	if len(activeLoans) == 0 {
		m.SendMessage(chatID, "У вас нет активных займов для возврата.")
		m.ShowMainMenu(chatID)
		return
	}

	// Display loans with inline keyboard for selection
	var keyboard [][]tgbotapi.InlineKeyboardButton
	for _, loan := range activeLoans {
		button := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("ID %d: %s - %d ₸", loan.ID, loan.Borrower, loan.Amount),
			fmt.Sprintf("repay_%d", loan.ID),
		)
		keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(button))
	}

	// Add back button
	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_main"),
	))

	msg := tgbotapi.NewMessage(chatID, "Выберите займ для отметки как возвращенный:")
	msg.ReplyMarkup = tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
	m.bot.Send(msg)

	// Set state for next step
	m.SetState(chatID, OpRepayLoan, 0)
}

// HandleAddLoanStep processes each step of the add loan flow
func (m *BotManager) HandleAddLoanStep(chatID int64, text string) {
	state := m.GetState(chatID)

	log.Printf("Handling add loan step %d for user %d with input: %s", state.Step, chatID, text)

	switch state.Step {
	case 0: // Getting borrower name
		if text == "" {
			m.SendMessage(chatID, "❌ Имя заемщика не может быть пустым. Пожалуйста, введите корректное имя:")
			return
		}

		// Save borrower name and move to next step
		m.SaveStateData(chatID, "borrower_name", text)
		m.SetState(chatID, OpAddLoan, 1)
		m.SendMessage(chatID, "💰 Введите сумму займа:")

	case 1: // Getting loan amount
		amount, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			m.SendMessage(chatID, "❌ Некорректная сумма. Пожалуйста, введите целое число:")
			return
		}

		// Save amount and move to next step
		m.SaveStateData(chatID, "amount", fmt.Sprintf("%d", amount))
		m.SetState(chatID, OpAddLoan, 2)
		m.SendMessage(chatID, "📝 Введите цель займа:")

	case 2: // Getting loan purpose
		if text == "" {
			m.SendMessage(chatID, "❌ Цель займа не может быть пустой. Пожалуйста, введите корректную цель:")
			return
		}

		// Save purpose and complete the process
		m.SaveStateData(chatID, "purpose", text)

		// Generate a new loan ID
		var newLoanID int
		err := m.db.QueryRow("SELECT COALESCE(MAX(loan_id), 0) + 1 FROM loans WHERE user_id = ?", chatID).Scan(&newLoanID)
		if err != nil {
			log.Printf("Error generating loan ID: %v", err)
			m.SendMessage(chatID, fmt.Sprintf("❌ Ошибка при создании ID займа: %v", err))
			return
		}

		// Insert the new loan into the database
		query := `INSERT INTO loans (user_id, loan_id, borrower_name, amount, purpose, repaid) 
				  VALUES (?, ?, ?, ?, ?, 0)`
		_, err = m.db.Exec(
			query,
			chatID,
			newLoanID,
			state.Data["borrower_name"],
			state.Data["amount"],
			state.Data["purpose"],
		)

		if err != nil {
			log.Printf("Error inserting loan: %v", err)
			m.SendMessage(chatID, fmt.Sprintf("❌ Не удалось зарегистрировать займ: %v", err))
			return
		}

		// Send success message
		successMsg := fmt.Sprintf(
			"✅ Займ успешно зарегистрирован!\n\n"+
				"👤 Заемщик: %s\n"+
				"💰 Сумма: %s ₸\n"+
				"🎯 Цель: %s\n"+
				"🆔 ID займа: %d\n\n"+
				"〰️〰️〰️〰️〰️〰️〰️〰️〰️〰️",
			state.Data["borrower_name"],
			state.Data["amount"],
			state.Data["purpose"],
			newLoanID,
		)
		m.SendMessage(chatID, successMsg)

		// Clear state and show main menu
		m.ClearState(chatID)
		m.ShowMainMenu(chatID)
	}
}

// HandleRepayLoanStep processes steps in the repay loan flow
func (m *BotManager) HandleRepayLoanStep(chatID int64, text string) {
	state := m.GetState(chatID)

	switch state.Step {
	case 0: // Select loan to repay
		// Try to parse loan ID
		loanID, err := strconv.Atoi(text)
		if err != nil {
			m.SendMessage(chatID, "❌ Пожалуйста, введите корректный номер займа из списка.")
			return
		}

		// Check if loan exists and is active
		var exists bool
		var borrower string
		var amount int64
		err = m.db.QueryRow(
			"SELECT EXISTS(SELECT 1 FROM loans WHERE user_id = ? AND loan_id = ? AND repaid = 0), borrower_name, amount FROM loans WHERE user_id = ? AND loan_id = ?",
			chatID, loanID, chatID, loanID,
		).Scan(&exists, &borrower, &amount)

		if err != nil {
			log.Printf("Error checking loan existence: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при проверке займа.")
			m.ClearState(chatID)
			m.ShowMainMenu(chatID)
			return
		}

		if !exists {
			m.SendMessage(chatID, "❌ Указанный займ не найден или уже погашен.")
			return
		}

		// Save loan ID and advance to next step
		m.SaveStateData(chatID, "loan_id", text)
		m.SaveStateData(chatID, "borrower", borrower)
		m.SaveStateData(chatID, "amount", fmt.Sprintf("%d", amount))
		m.SetState(chatID, OpRepayLoan, 1)

		// Ask for confirmation
		m.SendMessage(chatID, fmt.Sprintf(
			"Вы собираетесь отметить займ #%d от %s на сумму %d ₸ как возвращенный.\n\nВведите \"да\" для подтверждения или \"нет\" для отмены.",
			loanID, borrower, amount,
		))

	case 1: // Confirm repayment
		confirmation := strings.ToLower(text)

		if confirmation == "да" {
			// Get loan details from state
			loanIDStr, _ := m.GetStateData(chatID, "loan_id")
			borrower, _ := m.GetStateData(chatID, "borrower")
			amountStr, _ := m.GetStateData(chatID, "amount")

			loanID, _ := strconv.Atoi(loanIDStr)
			amount, _ := strconv.ParseInt(amountStr, 10, 64)

			// Mark loan as repaid
			_, err := m.db.Exec(
				"UPDATE loans SET repaid = 1 WHERE user_id = ? AND loan_id = ?",
				chatID, loanID,
			)

			if err != nil {
				log.Printf("Error marking loan as repaid: %v", err)
				m.SendMessage(chatID, "❌ Произошла ошибка при отметке займа как возвращенного.")
				m.ClearState(chatID)
				m.ShowMainMenu(chatID)
				return
			}

			// Insert into repayments table
			date := time.Now().Format("2006-01-02")
			_, err = m.db.Exec(
				"INSERT INTO repayments (user_id, loan_id, amount, repayment_date, note) VALUES (?, ?, ?, ?, 'Полный возврат')",
				chatID, loanID, amount, date,
			)

			if err != nil {
				log.Printf("Error recording repayment: %v", err)
				// Loan is already marked as repaid, so we proceed
			}

			// Send confirmation
			m.SendMessage(chatID, fmt.Sprintf(
				"✅ Займ #%d от %s на сумму %d ₸ отмечен как возвращенный!",
				loanID, borrower, amount,
			))

		} else if confirmation == "нет" {
			m.SendMessage(chatID, "❌ Отметка займа как возвращенного отменена.")
		} else {
			m.SendMessage(chatID, "Пожалуйста, введите \"да\" для подтверждения или \"нет\" для отмены.")
			return
		}

		// Clear state and show main menu
		m.ClearState(chatID)
		m.ShowMainMenu(chatID)
	}
}

// ShowBalance displays the user's active loans
func (m *BotManager) ShowBalance(chatID int64) {
	// Query active loans
	rows, err := m.db.Query(
		"SELECT loan_id, borrower_name, amount FROM loans WHERE user_id = ? AND repaid = 0",
		chatID,
	)

	if err != nil {
		log.Printf("Error querying loans: %v", err)
		m.SendMessage(chatID, fmt.Sprintf("❌ Ошибка при получении баланса: %v", err))
		return
	}
	defer rows.Close()

	// Build response
	var response strings.Builder
	response.WriteString("📊 Активные займы:\n\n")

	var totalAmount int64
	loanCount := 0

	// Process each loan
	for rows.Next() {
		var id int
		var borrower string
		var amount int64

		if err := rows.Scan(&id, &borrower, &amount); err != nil {
			log.Printf("Error scanning loan row: %v", err)
			continue
		}

		totalAmount += amount
		loanCount++

		response.WriteString(fmt.Sprintf(
			"🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n➖➖➖➖➖➖➖➖➖➖\n\n",
			id, borrower, amount,
		))
	}

	// Add summary
	if loanCount == 0 {
		response.WriteString("У вас нет активных займов! 🎉")
	} else {
		response.WriteString(fmt.Sprintf("💼 Общая сумма активных займов: %d ₸", totalAmount))
	}

	// Send response
	m.SendMessage(chatID, response.String())
}

// ShowStats displays lending statistics
func (m *BotManager) ShowStats(chatID int64) {
	var totalLoans int
	var totalLent int64
	var totalRepaid int

	// Get total loans and amount
	err := m.db.QueryRow(
		"SELECT COUNT(*), COALESCE(SUM(amount), 0) FROM loans WHERE user_id = ?",
		chatID,
	).Scan(&totalLoans, &totalLent)

	if err != nil {
		log.Printf("Error getting loan stats: %v", err)
		m.SendMessage(chatID, fmt.Sprintf("❌ Ошибка при формировании статистики: %v", err))
		return
	}

	// Get repaid count
	err = m.db.QueryRow(
		"SELECT COUNT(*) FROM loans WHERE user_id = ? AND repaid = 1",
		chatID,
	).Scan(&totalRepaid)

	if err != nil {
		log.Printf("Error getting repaid count: %v", err)
		m.SendMessage(chatID, fmt.Sprintf("❌ Ошибка при формировании статистики: %v", err))
		return
	}

	// Format stats message
	stats := fmt.Sprintf(
		"📈 Статистика займов:\n\n"+
			"🔢 Всего займов: %d\n"+
			"💰 Всего выдано: %d ₸\n"+
			"✅ Возвращено займов: %d\n"+
			"⏳ Ожидают возврата: %d\n\n"+
			"〰️〰️〰️〰️〰️〰️〰️〰️〰️〰️",
		totalLoans,
		totalLent,
		totalRepaid,
		totalLoans-totalRepaid,
	)

	// Send stats
	m.SendMessage(chatID, stats)
}

// ShowLoanManagementMenu displays options for managing loans
func (m *BotManager) ShowLoanManagementMenu(chatID int64) {
	menuButtons := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Редактировать займ", SubMenuEdit),
			tgbotapi.NewInlineKeyboardButtonData("🗑️ Удалить займ", SubMenuDelete),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💵 Частичный возврат", SubMenuPartial),
			tgbotapi.NewInlineKeyboardButtonData("📋 История платежей", SubMenuRepayments),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_main"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "✏️ Управление займами\nВыберите действие:")
	msg.ReplyMarkup = menuButtons
	_, err := m.bot.Send(msg)
	if err != nil {
		log.Printf("Error showing loan management menu: %v", err)
	}
}

// ShowSearchMenu displays search options
func (m *BotManager) ShowSearchMenu(chatID int64) {
	menuButtons := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👤 Поиск по имени", SearchByName),
			tgbotapi.NewInlineKeyboardButtonData("📊 По статусу", SearchByStatus),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Все займы", SearchAll),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_main"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "🔍 Поиск займов\nВыберите критерий поиска:")
	msg.ReplyMarkup = menuButtons
	_, err := m.bot.Send(msg)
	if err != nil {
		log.Printf("Error showing search menu: %v", err)
	}
}

// HandleCallbackQuery processes button presses
func (m *BotManager) HandleCallbackQuery(callback *tgbotapi.CallbackQuery) {
	// Acknowledge the button press
	callback_config := tgbotapi.NewCallback(callback.ID, "")
	m.bot.Send(callback_config)

	// Remove the keyboard to prevent multiple clicks
	editMsg := tgbotapi.NewEditMessageReplyMarkup(
		callback.Message.Chat.ID,
		callback.Message.MessageID,
		tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{},
		},
	)
	m.bot.Send(editMsg)

	// Get the callback data
	data := callback.Data
	chatID := callback.Message.Chat.ID

	// Switch based on the callback data
	switch {
	case data == MenuAddLoan:
		m.StartAddLoanFlow(chatID)
	case data == MenuRepay:
		m.StartRepayLoanFlow(chatID)
	case data == MenuBalance:
		m.ShowBalance(chatID)
	case data == MenuStats:
		m.ShowStats(chatID)
	case data == MenuManage:
		m.ShowLoanManagementMenu(chatID)
	case data == MenuSearch:
		m.ShowSearchMenu(chatID)
	case data == "back_to_manage":
		m.ShowLoanManagementMenu(chatID)
	case data == "back_to_search":
		m.ShowSearchMenu(chatID)
	case data == "back_to_main":
		m.ShowMainMenu(chatID)
	case data == "menu_edit_loan":
		m.StartEditLoanFlow(chatID)
	case data == "menu_delete_loan":
		m.StartDeleteLoanFlow(chatID)
	case data == "menu_partial_repay":
		m.StartPartialRepaymentFlow(chatID)
	case data == "menu_repayment_history":
		m.ShowRepaymentHistory(chatID)
	case data == "search_by_name":
		m.StartSearchByNameFlow(chatID)
	case data == "search_by_status":
		m.StartSearchByStatusFlow(chatID)
	case data == "search_all_loans":
		m.ShowAllLoans(chatID)
	case data == "status_active":
		m.ShowLoansByStatus(chatID, false)
	case data == "status_repaid":
		m.ShowLoansByStatus(chatID, true)
	case strings.HasPrefix(data, "edit_"):
		// Extract loan ID from callback data (format: "edit_123")
		loanIDStr := strings.TrimPrefix(data, "edit_")
		loanID, err := strconv.Atoi(loanIDStr)
		if err != nil {
			log.Printf("Error converting loan ID: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при выборе займа.")
			m.ShowMainMenu(chatID)
			return
		}

		// Get loan details
		loan, err := m.GetLoanByID(chatID, loanID)
		if err != nil {
			log.Printf("Error getting loan details: %v", err)
			m.SendMessage(chatID, "❌ Не удалось получить информацию о займе.")
			m.ShowMainMenu(chatID)
			return
		}

		// Display edit options
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("👤 Изменить имя", fmt.Sprintf("edit_name_%d", loanID)),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("💰 Изменить сумму", fmt.Sprintf("edit_amount_%d", loanID)),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("📝 Изменить цель", fmt.Sprintf("edit_purpose_%d", loanID)),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_manage"),
			),
		)

		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(
			"🔍 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n📝 Цель: %s\n\nВыберите, что хотите изменить:",
			loan.ID, loan.Borrower, loan.Amount, loan.Purpose,
		))
		msg.ReplyMarkup = keyboard
		m.bot.Send(msg)

	case strings.HasPrefix(data, "edit_name_"):
		// Extract loan ID from callback data (format: "edit_name_123")
		loanIDStr := strings.TrimPrefix(data, "edit_name_")

		// Save the loan ID and set the operation state
		m.SaveStateData(chatID, "loan_id", loanIDStr)
		m.SaveStateData(chatID, "edit_field", "name")
		m.SetState(chatID, OpEditLoan, 1)

		// Prompt for new name
		m.SendMessage(chatID, "Введите новое имя заемщика:")

	case strings.HasPrefix(data, "edit_amount_"):
		// Extract loan ID from callback data (format: "edit_amount_123")
		loanIDStr := strings.TrimPrefix(data, "edit_amount_")

		// Save the loan ID and set the operation state
		m.SaveStateData(chatID, "loan_id", loanIDStr)
		m.SaveStateData(chatID, "edit_field", "amount")
		m.SetState(chatID, OpEditLoan, 1)

		// Prompt for new amount
		m.SendMessage(chatID, "Введите новую сумму займа (целое число):")

	case strings.HasPrefix(data, "edit_purpose_"):
		// Extract loan ID from callback data (format: "edit_purpose_123")
		loanIDStr := strings.TrimPrefix(data, "edit_purpose_")

		// Save the loan ID and set the operation state
		m.SaveStateData(chatID, "loan_id", loanIDStr)
		m.SaveStateData(chatID, "edit_field", "purpose")
		m.SetState(chatID, OpEditLoan, 1)

		// Prompt for new purpose
		m.SendMessage(chatID, "Введите новую цель займа:")

	case strings.HasPrefix(data, "delete_"):
		// Extract loan ID from callback data (format: "delete_123")
		loanIDStr := strings.TrimPrefix(data, "delete_")
		loanID, err := strconv.Atoi(loanIDStr)
		if err != nil {
			log.Printf("Error converting loan ID: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при выборе займа.")
			m.ShowMainMenu(chatID)
			return
		}

		// Get loan details
		loan, err := m.GetLoanByID(chatID, loanID)
		if err != nil {
			log.Printf("Error getting loan details: %v", err)
			m.SendMessage(chatID, "❌ Не удалось получить информацию о займе.")
			m.ShowMainMenu(chatID)
			return
		}

		// Display confirmation
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ Да, удалить", fmt.Sprintf("confirm_delete_%d", loanID)),
				tgbotapi.NewInlineKeyboardButtonData("❌ Нет, отмена", "back_to_manage"),
			),
		)

		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(
			"⚠️ ВНИМАНИЕ! Вы собираетесь удалить займ:\n\n🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n📝 Цель: %s\n\nЭто действие нельзя будет отменить. Вы уверены?",
			loan.ID, loan.Borrower, loan.Amount, loan.Purpose,
		))
		msg.ReplyMarkup = keyboard
		m.bot.Send(msg)

	case strings.HasPrefix(data, "confirm_delete_"):
		// Extract loan ID from callback data (format: "confirm_delete_123")
		loanIDStr := strings.TrimPrefix(data, "confirm_delete_")
		loanID, err := strconv.Atoi(loanIDStr)
		if err != nil {
			log.Printf("Error converting loan ID: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при удалении займа.")
			m.ShowMainMenu(chatID)
			return
		}

		// Delete the loan
		err = m.DeleteLoan(chatID, loanID)
		if err != nil {
			log.Printf("Error deleting loan: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при удалении займа.")
		} else {
			m.SendMessage(chatID, "✅ Займ успешно удален!")
		}

		m.ShowMainMenu(chatID)

	case strings.HasPrefix(data, "partial_"):
		// Extract loan ID from callback data (format: "partial_123")
		loanIDStr := strings.TrimPrefix(data, "partial_")
		loanID, err := strconv.Atoi(loanIDStr)
		if err != nil {
			log.Printf("Error converting loan ID: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при выборе займа.")
			m.ShowMainMenu(chatID)
			return
		}

		// Get loan details
		loan, err := m.GetLoanByID(chatID, loanID)
		if err != nil {
			log.Printf("Error getting loan details: %v", err)
			m.SendMessage(chatID, "❌ Не удалось получить информацию о займе.")
			m.ShowMainMenu(chatID)
			return
		}

		// Calculate remaining amount
		repaidAmount := m.GetTotalRepaidAmount(chatID, loanID)
		remainingAmount := loan.Amount - repaidAmount

		// Save the loan ID and set the operation state
		m.SaveStateData(chatID, "loan_id", loanIDStr)
		m.SaveStateData(chatID, "remaining_amount", fmt.Sprintf("%d", remainingAmount))
		m.SetState(chatID, OpPartialRepay, 1)

		// Prompt for repayment amount
		m.SendMessage(chatID, fmt.Sprintf(
			"Займ: #%d от %s\nОсталось выплатить: %d ₸\n\nВведите сумму частичного возврата (целое число):",
			loan.ID, loan.Borrower, remainingAmount,
		))

	case strings.HasPrefix(data, "history_"):
		// Extract loan ID from callback data (format: "history_123")
		loanIDStr := strings.TrimPrefix(data, "history_")
		loanID, err := strconv.Atoi(loanIDStr)
		if err != nil {
			log.Printf("Error converting loan ID: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при просмотре истории.")
			m.ShowMainMenu(chatID)
			return
		}

		// Show repayment history for this loan
		m.ShowLoanRepaymentHistory(chatID, loanID)

	case strings.HasPrefix(data, "repay_"):
		// Extract loan ID from callback data (format: "repay_123")
		loanIDStr := strings.TrimPrefix(data, "repay_")
		loanID, err := strconv.Atoi(loanIDStr)
		if err != nil {
			log.Printf("Error converting loan ID: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при выборе займа.")
			m.ShowMainMenu(chatID)
			return
		}

		// Get loan details
		loan, err := m.GetLoanByID(chatID, loanID)
		if err != nil {
			log.Printf("Error getting loan details: %v", err)
			m.SendMessage(chatID, "❌ Не удалось получить информацию о займе.")
			m.ShowMainMenu(chatID)
			return
		}

		// Display confirmation
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ Да, подтверждаю", fmt.Sprintf("confirm_repay_%d", loanID)),
				tgbotapi.NewInlineKeyboardButtonData("❌ Нет, отмена", "back_to_main"),
			),
		)

		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(
			"Вы собираетесь отметить займ как возвращенный:\n\n🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n📝 Цель: %s\n\nПодтверждаете?",
			loan.ID, loan.Borrower, loan.Amount, loan.Purpose,
		))
		msg.ReplyMarkup = keyboard
		m.bot.Send(msg)

	case strings.HasPrefix(data, "confirm_repay_"):
		// Extract loan ID from callback data (format: "confirm_repay_123")
		loanIDStr := strings.TrimPrefix(data, "confirm_repay_")
		loanID, err := strconv.Atoi(loanIDStr)
		if err != nil {
			log.Printf("Error converting loan ID: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при подтверждении возврата.")
			m.ShowMainMenu(chatID)
			return
		}

		// Get loan details
		loan, err := m.GetLoanByID(chatID, loanID)
		if err != nil {
			log.Printf("Error getting loan details: %v", err)
			m.SendMessage(chatID, "❌ Не удалось получить информацию о займе.")
			m.ShowMainMenu(chatID)
			return
		}

		// Mark loan as repaid
		_, err = m.db.Exec(
			"UPDATE loans SET repaid = 1 WHERE user_id = ? AND loan_id = ?",
			chatID, loanID,
		)
		if err != nil {
			log.Printf("Error marking loan as repaid: %v", err)
			m.SendMessage(chatID, "❌ Произошла ошибка при отметке займа как возвращенного.")
			m.ShowMainMenu(chatID)
			return
		}

		// Insert into repayments table
		date := time.Now().Format("2006-01-02")
		_, err = m.db.Exec(
			"INSERT INTO repayments (user_id, loan_id, amount, repayment_date, note) VALUES (?, ?, ?, ?, 'Полный возврат')",
			chatID, loanID, loan.Amount, date,
		)
		if err != nil {
			log.Printf("Error recording repayment: %v", err)
			// Loan is already marked as repaid, so we proceed
		}

		// Send confirmation
		m.SendMessage(chatID, fmt.Sprintf(
			"✅ Займ #%d от %s на сумму %d ₸ отмечен как возвращенный!",
			loan.ID, loan.Borrower, loan.Amount,
		))

		m.ShowMainMenu(chatID)

	default:
		m.SendMessage(chatID, "❓ Неизвестная команда")
		m.ShowMainMenu(chatID)
	}
}

// ShowLoansByStatus displays loans filtered by repaid status
func (m *BotManager) ShowLoansByStatus(chatID int64, repaidStatus bool) {
	rows, err := m.db.Query(
		"SELECT loan_id, borrower_name, amount, purpose FROM loans WHERE user_id = ? AND repaid = ?",
		chatID, repaidStatus,
	)
	if err != nil {
		log.Printf("Error getting loans by status: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить список займов.")
		m.ShowMainMenu(chatID)
		return
	}
	defer rows.Close()

	var loans []Loan
	for rows.Next() {
		var loan Loan
		loan.UserID = chatID
		loan.Repaid = repaidStatus

		if err := rows.Scan(&loan.ID, &loan.Borrower, &loan.Amount, &loan.Purpose); err != nil {
			log.Printf("Error scanning loan: %v", err)
			continue
		}

		loans = append(loans, loan)
	}

	if len(loans) == 0 {
		status := "возвращенных"
		if !repaidStatus {
			status = "активных"
		}
		m.SendMessage(chatID, fmt.Sprintf("У вас нет %s займов.", status))
		m.ShowMainMenu(chatID)
		return
	}

	// Build response
	var response strings.Builder
	status := "✅ Возвращенные"
	if !repaidStatus {
		status = "⏳ Активные"
	}
	response.WriteString(fmt.Sprintf("📋 %s займы:\n\n", status))

	for _, loan := range loans {
		if !loan.Repaid {
			// Calculate remaining amount for active loans
			repaidAmount := m.GetTotalRepaidAmount(chatID, loan.ID)
			remainingAmount := loan.Amount - repaidAmount

			response.WriteString(fmt.Sprintf(
				"🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n💵 Остаток: %d ₸\n📝 Цель: %s\n➖➖➖➖➖➖➖➖➖➖\n\n",
				loan.ID, loan.Borrower, loan.Amount, remainingAmount, loan.Purpose,
			))
		} else {
			response.WriteString(fmt.Sprintf(
				"🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n📝 Цель: %s\n➖➖➖➖➖➖➖➖➖➖\n\n",
				loan.ID, loan.Borrower, loan.Amount, loan.Purpose,
			))
		}
	}

	// Send response
	m.SendMessage(chatID, response.String())
	m.ShowMainMenu(chatID)
}

// GetLoanByID retrieves a loan by its ID
func (m *BotManager) GetLoanByID(chatID int64, loanID int) (Loan, error) {
	var loan Loan
	loan.UserID = chatID
	loan.ID = loanID

	err := m.db.QueryRow(
		"SELECT borrower_name, amount, purpose, repaid FROM loans WHERE user_id = ? AND loan_id = ?",
		chatID, loanID,
	).Scan(&loan.Borrower, &loan.Amount, &loan.Purpose, &loan.Repaid)

	if err != nil {
		return Loan{}, err
	}

	return loan, nil
}

// DeleteLoan removes a loan and its repayments from the database
func (m *BotManager) DeleteLoan(chatID int64, loanID int) error {
	// Start a transaction
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}

	// Delete repayments first (due to foreign key constraints)
	_, err = tx.Exec("DELETE FROM repayments WHERE user_id = ? AND loan_id = ?", chatID, loanID)
	if err != nil {
		tx.Rollback()
		return err
	}

	// Delete the loan
	_, err = tx.Exec("DELETE FROM loans WHERE user_id = ? AND loan_id = ?", chatID, loanID)
	if err != nil {
		tx.Rollback()
		return err
	}

	// Commit the transaction
	return tx.Commit()
}

// ShowLoanRepaymentHistory displays the repayment history for a specific loan
func (m *BotManager) ShowLoanRepaymentHistory(chatID int64, loanID int) {
	// Get loan details
	loan, err := m.GetLoanByID(chatID, loanID)
	if err != nil {
		log.Printf("Error getting loan details: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить информацию о займе.")
		m.ShowMainMenu(chatID)
		return
	}

	// Get repayment history
	rows, err := m.db.Query(
		"SELECT amount, repayment_date, note FROM repayments WHERE user_id = ? AND loan_id = ? ORDER BY repayment_date",
		chatID, loanID,
	)
	if err != nil {
		log.Printf("Error getting repayment history: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить историю платежей.")
		m.ShowMainMenu(chatID)
		return
	}
	defer rows.Close()

	// Build response
	var response strings.Builder
	response.WriteString(fmt.Sprintf("📋 История платежей по займу #%d:\n\n", loanID))
	response.WriteString(fmt.Sprintf("👤 Заемщик: %s\n", loan.Borrower))
	response.WriteString(fmt.Sprintf("💰 Общая сумма: %d ₸\n\n", loan.Amount))

	// Calculate total repaid
	var totalRepaid int64
	var repayments []struct {
		Amount int64
		Date   string
		Note   string
	}

	for rows.Next() {
		var amount int64
		var date string
		var note string

		if err := rows.Scan(&amount, &date, &note); err != nil {
			log.Printf("Error scanning repayment: %v", err)
			continue
		}

		totalRepaid += amount
		repayments = append(repayments, struct {
			Amount int64
			Date   string
			Note   string
		}{
			Amount: amount,
			Date:   date,
			Note:   note,
		})
	}

	// Display individual repayments
	if len(repayments) == 0 {
		response.WriteString("Нет записей о платежах по этому займу.\n")
	} else {
		for i, repayment := range repayments {
			noteDisplay := ""
			if repayment.Note != "" {
				noteDisplay = fmt.Sprintf("\n📝 Примечание: %s", repayment.Note)
			}

			response.WriteString(fmt.Sprintf(
				"%d. 📅 %s\n💵 Сумма: %d ₸%s\n\n",
				i+1, repayment.Date, repayment.Amount, noteDisplay,
			))
		}
	}

	// Add summary
	remainingAmount := loan.Amount - totalRepaid
	status := "✅ Возвращен полностью"
	if !loan.Repaid {
		status = fmt.Sprintf("⏳ Остаток: %d ₸", remainingAmount)
	}

	response.WriteString(fmt.Sprintf(
		"💵 Итого выплачено: %d ₸\n📊 Статус: %s",
		totalRepaid, status,
	))

	// Send response and show back button
	m.SendMessage(chatID, response.String())

	// Provide a button to go back
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_manage"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "Выберите действие:")
	msg.ReplyMarkup = keyboard
	m.bot.Send(msg)
}

// StartSearchByNameFlow begins the process of searching for loans by borrower name
func (m *BotManager) StartSearchByNameFlow(chatID int64) {
	// First clear any existing state
	m.ClearState(chatID)

	// Set state for search by name
	m.SetState(chatID, OpSearchLoan, 0)
	m.SaveStateData(chatID, "search_type", "by_name")

	// Send prompt for borrower name
	m.SendMessage(chatID, "Введите имя заемщика для поиска:")
}

// StartSearchByStatusFlow begins the process of searching for loans by status
func (m *BotManager) StartSearchByStatusFlow(chatID int64) {
	// Create inline keyboard for status selection
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⏳ Активные", "status_active"),
			tgbotapi.NewInlineKeyboardButtonData("✅ Возвращенные", "status_repaid"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_search"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "Выберите статус займов для поиска:")
	msg.ReplyMarkup = keyboard
	m.bot.Send(msg)
}

// ShowAllLoans displays all loans for a user
func (m *BotManager) ShowAllLoans(chatID int64) {
	allLoans, err := m.GetAllLoansForUser(chatID)
	if err != nil {
		log.Printf("Error getting loans: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить список займов.")
		m.ShowMainMenu(chatID)
		return
	}

	if len(allLoans) == 0 {
		m.SendMessage(chatID, "У вас нет займов.")
		m.ShowMainMenu(chatID)
		return
	}

	// Build response
	var response strings.Builder
	response.WriteString("📋 Все займы:\n\n")

	for _, loan := range allLoans {
		status := "✅ Возвращен"
		if !loan.Repaid {
			status = "⏳ Активен"

			// Calculate remaining amount for active loans
			repaidAmount := m.GetTotalRepaidAmount(chatID, loan.ID)
			remainingAmount := loan.Amount - repaidAmount

			response.WriteString(fmt.Sprintf(
				"🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n💵 Остаток: %d ₸\n📝 Цель: %s\n📊 Статус: %s\n➖➖➖➖➖➖➖➖➖➖\n\n",
				loan.ID, loan.Borrower, loan.Amount, remainingAmount, loan.Purpose, status,
			))
		} else {
			response.WriteString(fmt.Sprintf(
				"🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n📝 Цель: %s\n📊 Статус: %s\n➖➖➖➖➖➖➖➖➖➖\n\n",
				loan.ID, loan.Borrower, loan.Amount, loan.Purpose, status,
			))
		}
	}

	// Send response
	m.SendMessage(chatID, response.String())
	m.ShowMainMenu(chatID)
}

// Loan represents a loan record
type Loan struct {
	ID       int
	UserID   int64
	Borrower string
	Amount   int64
	Purpose  string
	Repaid   bool
}

// GetActiveLoansForUser retrieves all active loans for a user
func (m *BotManager) GetActiveLoansForUser(chatID int64) ([]Loan, error) {
	rows, err := m.db.Query(
		"SELECT loan_id, borrower_name, amount, purpose FROM loans WHERE user_id = ? AND repaid = 0",
		chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var loans []Loan
	for rows.Next() {
		var loan Loan
		loan.UserID = chatID
		loan.Repaid = false

		if err := rows.Scan(&loan.ID, &loan.Borrower, &loan.Amount, &loan.Purpose); err != nil {
			return nil, err
		}

		loans = append(loans, loan)
	}

	return loans, nil
}

// GetAllLoansForUser retrieves all loans for a user
func (m *BotManager) GetAllLoansForUser(chatID int64) ([]Loan, error) {
	rows, err := m.db.Query(
		"SELECT loan_id, borrower_name, amount, purpose, repaid FROM loans WHERE user_id = ?",
		chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var loans []Loan
	for rows.Next() {
		var loan Loan
		loan.UserID = chatID

		if err := rows.Scan(&loan.ID, &loan.Borrower, &loan.Amount, &loan.Purpose, &loan.Repaid); err != nil {
			return nil, err
		}

		loans = append(loans, loan)
	}

	return loans, nil
}

// GetTotalRepaidAmount calculates the total amount repaid for a loan
func (m *BotManager) GetTotalRepaidAmount(chatID int64, loanID int) int64 {
	var totalRepaid int64
	err := m.db.QueryRow(
		"SELECT COALESCE(SUM(amount), 0) FROM repayments WHERE user_id = ? AND loan_id = ?",
		chatID, loanID,
	).Scan(&totalRepaid)

	if err != nil {
		log.Printf("Error calculating repaid amount: %v", err)
		return 0
	}

	return totalRepaid
}

// Start runs the bot and begins processing updates
func (m *BotManager) Start() {
	log.Println("Starting bot...")

	// Configure update channel
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := m.bot.GetUpdatesChan(u)

	// Start reminder scheduler
	m.StartReminderScheduler()

	// Process updates
	for update := range updates {
		// Skip already processed updates
		if update.UpdateID <= m.lastProcessedID {
			continue
		}
		m.lastProcessedID = update.UpdateID

		// Process callback queries (button presses)
		if update.CallbackQuery != nil {
			m.HandleCallbackQuery(update.CallbackQuery)
			continue
		}

		// Process messages
		if update.Message != nil && update.Message.Text != "" {
			m.HandleMessage(update.Message)
		}
	}
}

// StartReminderScheduler sends weekly reminders about outstanding loans
func (m *BotManager) StartReminderScheduler() {
	go func() {
		ticker := time.NewTicker(7 * 24 * time.Hour)
		for {
			<-ticker.C
			m.SendReminders()
		}
	}()
}

// SendReminders sends reminder messages to users with outstanding loans
func (m *BotManager) SendReminders() {
	// Get distinct users with active loans
	rows, err := m.db.Query("SELECT DISTINCT user_id FROM loans WHERE repaid = 0")
	if err != nil {
		log.Printf("Error querying users for reminders: %v", err)
		return
	}
	defer rows.Close()

	// Build list of users
	var userIDs []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			log.Printf("Error scanning user ID: %v", err)
			continue
		}
		userIDs = append(userIDs, userID)
	}

	// Send reminders to each user
	for _, userID := range userIDs {
		// Get active loans for this user
		loanRows, err := m.db.Query(
			"SELECT loan_id, borrower_name, amount FROM loans WHERE user_id = ? AND repaid = 0",
			userID,
		)
		if err != nil {
			log.Printf("Error querying loans for user %d: %v", userID, err)
			continue
		}

		// Build reminder message
		reminderMsg := "⏰ Еженедельное напоминание: У вас есть активные займы:\n\n"

		for loanRows.Next() {
			var id int
			var borrower string
			var amount int64

			if err := loanRows.Scan(&id, &borrower, &amount); err != nil {
				log.Printf("Error scanning loan: %v", err)
				continue
			}

			reminderMsg += fmt.Sprintf("🆔 Займ #%d - %s: %d ₸\n", id, borrower, amount)
		}
		loanRows.Close()

		// Send the reminder
		m.SendMessage(userID, reminderMsg)
	}
}

// HandleMessage processes text messages
func (m *BotManager) HandleMessage(message *tgbotapi.Message) {
	chatID := message.Chat.ID
	text := strings.TrimSpace(message.Text)

	log.Printf("Message from user %d: %s", chatID, text)

	// Handle commands
	if message.IsCommand() {
		switch message.Command() {
		case "start":
			m.ClearState(chatID)
			m.ShowMainMenu(chatID)
		default:
			m.SendMessage(chatID, "🤔 Неизвестная команда. Используйте /start для начала работы.")
		}
		return
	}

	// Handle conversation state
	state := m.GetState(chatID)

	switch state.Operation {
	case OpAddLoan:
		m.HandleAddLoanStep(chatID, text)
	case OpRepayLoan:
		m.HandleRepayLoanStep(chatID, text)
	case OpEditLoan:
		m.HandleEditLoanStep(chatID, text)
	case OpPartialRepay:
		m.HandlePartialRepaymentStep(chatID, text)
	case OpSearchLoan:
		m.HandleSearchStep(chatID, text)
	case OpNone: // No active conversation
		m.ShowMainMenu(chatID)
	default:
		log.Printf("Unknown operation: %s", state.Operation)
		m.ShowMainMenu(chatID)
	}
}

// HandleEditLoanStep processes user input for the loan editing flow
func (m *BotManager) HandleEditLoanStep(chatID int64, text string) {
	state := m.GetState(chatID)

	// Get stored loan ID and edit field
	loanIDStr, _ := m.GetStateData(chatID, "loan_id")
	loanID, err := strconv.Atoi(loanIDStr)
	if err != nil {
		log.Printf("Error converting loan ID: %v", err)
		m.SendMessage(chatID, "❌ Произошла ошибка при редактировании займа.")
		m.ClearState(chatID)
		m.ShowMainMenu(chatID)
		return
	}

	editField, _ := m.GetStateData(chatID, "edit_field")

	switch state.Step {
	case 1: // Edit field
		// Update the specified field
		switch editField {
		case "name":
			// Update borrower name
			_, err := m.db.Exec(
				"UPDATE loans SET borrower_name = ? WHERE user_id = ? AND loan_id = ?",
				text, chatID, loanID,
			)
			if err != nil {
				log.Printf("Error updating loan name: %v", err)
				m.SendMessage(chatID, "❌ Не удалось обновить имя заемщика.")
				m.ClearState(chatID)
				m.ShowMainMenu(chatID)
				return
			}

			m.SendMessage(chatID, fmt.Sprintf("✅ Имя заемщика успешно изменено на \"%s\"!", text))

		case "amount":
			// Parse and validate amount
			amount, err := strconv.ParseInt(text, 10, 64)
			if err != nil || amount <= 0 {
				m.SendMessage(chatID, "❌ Пожалуйста, введите корректную сумму (целое положительное число).")
				return
			}

			// Update amount
			_, err = m.db.Exec(
				"UPDATE loans SET amount = ? WHERE user_id = ? AND loan_id = ?",
				amount, chatID, loanID,
			)
			if err != nil {
				log.Printf("Error updating loan amount: %v", err)
				m.SendMessage(chatID, "❌ Не удалось обновить сумму займа.")
				m.ClearState(chatID)
				m.ShowMainMenu(chatID)
				return
			}

			m.SendMessage(chatID, fmt.Sprintf("✅ Сумма займа успешно изменена на %d ₸!", amount))

		case "purpose":
			// Update purpose
			_, err := m.db.Exec(
				"UPDATE loans SET purpose = ? WHERE user_id = ? AND loan_id = ?",
				text, chatID, loanID,
			)
			if err != nil {
				log.Printf("Error updating loan purpose: %v", err)
				m.SendMessage(chatID, "❌ Не удалось обновить цель займа.")
				m.ClearState(chatID)
				m.ShowMainMenu(chatID)
				return
			}

			m.SendMessage(chatID, fmt.Sprintf("✅ Цель займа успешно изменена на \"%s\"!", text))

		default:
			log.Printf("Unknown edit field: %s", editField)
			m.SendMessage(chatID, "❌ Произошла ошибка при редактировании займа.")
		}

		// Clear state and show main menu
		m.ClearState(chatID)
		m.ShowMainMenu(chatID)
	}
}

// HandlePartialRepaymentStep processes user input for the partial repayment flow
func (m *BotManager) HandlePartialRepaymentStep(chatID int64, text string) {
	state := m.GetState(chatID)

	// Get stored loan ID and remaining amount
	loanIDStr, _ := m.GetStateData(chatID, "loan_id")
	loanID, err := strconv.Atoi(loanIDStr)
	if err != nil {
		log.Printf("Error converting loan ID: %v", err)
		m.SendMessage(chatID, "❌ Произошла ошибка при обработке частичного возврата.")
		m.ClearState(chatID)
		m.ShowMainMenu(chatID)
		return
	}

	remainingStr, _ := m.GetStateData(chatID, "remaining_amount")
	remaining, _ := strconv.ParseInt(remainingStr, 10, 64)

	switch state.Step {
	case 1: // Enter repayment amount
		// Parse and validate amount
		amount, err := strconv.ParseInt(text, 10, 64)
		if err != nil || amount <= 0 {
			m.SendMessage(chatID, "❌ Пожалуйста, введите корректную сумму (целое положительное число).")
			return
		}

		// Check if amount exceeds remaining balance
		if amount > remaining {
			m.SendMessage(chatID, fmt.Sprintf(
				"❌ Сумма возврата (%d ₸) превышает остаток по займу (%d ₸).\nПожалуйста, введите корректную сумму или используйте полный возврат займа.",
				amount, remaining,
			))
			return
		}

		// Save repayment amount and ask for optional note
		m.SaveStateData(chatID, "repayment_amount", fmt.Sprintf("%d", amount))
		m.SetState(chatID, OpPartialRepay, 2)

		// Prompt for optional note
		m.SendMessage(chatID, "Введите примечание к платежу (или отправьте \"-\" чтобы пропустить):")

	case 2: // Enter note
		// Get the repayment amount
		amountStr, _ := m.GetStateData(chatID, "repayment_amount")
		amount, _ := strconv.ParseInt(amountStr, 10, 64)

		// Process note
		note := text
		if note == "-" {
			note = ""
		}

		// Record the repayment in the database
		date := time.Now().Format("2006-01-02")
		_, err := m.db.Exec(
			"INSERT INTO repayments (user_id, loan_id, amount, repayment_date, note) VALUES (?, ?, ?, ?, ?)",
			chatID, loanID, amount, date, note,
		)
		if err != nil {
			log.Printf("Error recording partial repayment: %v", err)
			m.SendMessage(chatID, "❌ Не удалось записать частичный возврат займа.")
			m.ClearState(chatID)
			m.ShowMainMenu(chatID)
			return
		}

		// Check if the loan is now fully repaid
		newRemaining := remaining - amount
		if newRemaining == 0 {
			// Mark loan as repaid
			_, err := m.db.Exec(
				"UPDATE loans SET repaid = 1 WHERE user_id = ? AND loan_id = ?",
				chatID, loanID,
			)
			if err != nil {
				log.Printf("Error updating loan status: %v", err)
			}

			m.SendMessage(chatID, fmt.Sprintf(
				"✅ Частичный возврат в размере %d ₸ записан!\nПоздравляем! Займ полностью погашен! 🎉",
				amount,
			))
		} else {
			m.SendMessage(chatID, fmt.Sprintf(
				"✅ Частичный возврат в размере %d ₸ записан!\nОстаток по займу: %d ₸",
				amount, newRemaining,
			))
		}

		// Clear state and show main menu
		m.ClearState(chatID)
		m.ShowMainMenu(chatID)
	}
}

// HandleSearchStep processes user input for the search flow
func (m *BotManager) HandleSearchStep(chatID int64, text string) {
	state := m.GetState(chatID)

	// Get search type
	searchType, _ := m.GetStateData(chatID, "search_type")

	switch state.Step {
	case 0: // Search by name
		if searchType == "by_name" {
			// Search loans by borrower name
			searchName := "%" + text + "%"
			rows, err := m.db.Query(
				"SELECT loan_id, borrower_name, amount, purpose, repaid FROM loans WHERE user_id = ? AND borrower_name LIKE ?",
				chatID, searchName,
			)
			if err != nil {
				log.Printf("Error searching loans: %v", err)
				m.SendMessage(chatID, "❌ Не удалось выполнить поиск.")
				m.ClearState(chatID)
				m.ShowMainMenu(chatID)
				return
			}
			defer rows.Close()

			// Process results
			var loans []Loan
			for rows.Next() {
				var loan Loan
				loan.UserID = chatID

				if err := rows.Scan(&loan.ID, &loan.Borrower, &loan.Amount, &loan.Purpose, &loan.Repaid); err != nil {
					log.Printf("Error scanning loan: %v", err)
					continue
				}

				loans = append(loans, loan)
			}

			// Display results
			if len(loans) == 0 {
				m.SendMessage(chatID, fmt.Sprintf("🔍 По запросу \"%s\" ничего не найдено.", text))
			} else {
				var response strings.Builder
				response.WriteString(fmt.Sprintf("🔍 Результаты поиска по \"%s\":\n\n", text))

				for _, loan := range loans {
					status := "✅ Возвращен"
					if !loan.Repaid {
						status = "⏳ Активен"

						// Calculate remaining amount for active loans
						repaidAmount := m.GetTotalRepaidAmount(chatID, loan.ID)
						remainingAmount := loan.Amount - repaidAmount

						response.WriteString(fmt.Sprintf(
							"🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n💵 Остаток: %d ₸\n📝 Цель: %s\n📊 Статус: %s\n➖➖➖➖➖➖➖➖➖➖\n\n",
							loan.ID, loan.Borrower, loan.Amount, remainingAmount, loan.Purpose, status,
						))
					} else {
						response.WriteString(fmt.Sprintf(
							"🆔 Займ #%d\n👤 Заемщик: %s\n💰 Сумма: %d ₸\n📝 Цель: %s\n📊 Статус: %s\n➖➖➖➖➖➖➖➖➖➖\n\n",
							loan.ID, loan.Borrower, loan.Amount, loan.Purpose, status,
						))
					}
				}

				m.SendMessage(chatID, response.String())
			}

			// Clear state and show main menu
			m.ClearState(chatID)
			m.ShowMainMenu(chatID)
		}
	}
}

// GetStateData retrieves data stored in the user state
func (m *BotManager) GetStateData(chatID int64, key string) (string, bool) {
	m.stateMutex.RLock()
	defer m.stateMutex.RUnlock()

	state, exists := m.userStates[chatID]
	if !exists {
		return "", false
	}

	value, exists := state.Data[key]
	return value, exists
}

func main() {
	// Get bot token from environment
	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN environment variable not set")
	}

	// Initialize Telegram bot
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("Failed to initialize bot: %v", err)
	}
	log.Printf("Authorized as @%s", bot.Self.UserName)

	// Open database connection
	db, err := sql.Open("sqlite", "./lending.db")
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer db.Close()

	// Initialize database schema
	if err := initializeDatabase(db); err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}

	// Create and start bot manager
	manager := NewBotManager(bot, db)
	manager.Start()
}

// Initialize database schema
func initializeDatabase(db *sql.DB) error {
	// Create or update the loans table
	loansTableSQL := `
	CREATE TABLE IF NOT EXISTS loans (
		user_id INTEGER NOT NULL,
		loan_id INTEGER NOT NULL,
		borrower_name TEXT NOT NULL,
		amount INTEGER NOT NULL,
		purpose TEXT,
		repaid BOOLEAN DEFAULT 0,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (user_id, loan_id)
	);`

	// Create the repayments table to track payment history
	repaymentsTableSQL := `
	CREATE TABLE IF NOT EXISTS repayments (
		repayment_id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		loan_id INTEGER NOT NULL,
		amount INTEGER NOT NULL,
		repayment_date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		note TEXT,
		FOREIGN KEY (user_id, loan_id) REFERENCES loans(user_id, loan_id)
	);`

	// Execute the SQL statements
	_, err := db.Exec(loansTableSQL)
	if err != nil {
		return fmt.Errorf("error creating loans table: %v", err)
	}

	_, err = db.Exec(repaymentsTableSQL)
	if err != nil {
		return fmt.Errorf("error creating repayments table: %v", err)
	}

	log.Println("Database tables created successfully")
	return nil
}

// StartEditLoanFlow begins the process of editing a loan
func (m *BotManager) StartEditLoanFlow(chatID int64) {
	// First clear any existing state
	m.ClearState(chatID)

	// Show active loans to select from
	activeLoans, err := m.GetActiveLoansForUser(chatID)
	if err != nil {
		log.Printf("Error getting active loans: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить список активных займов.")
		m.ShowMainMenu(chatID)
		return
	}

	if len(activeLoans) == 0 {
		m.SendMessage(chatID, "У вас нет активных займов для редактирования.")
		m.ShowMainMenu(chatID)
		return
	}

	// Display loans with inline keyboard for selection
	var keyboard [][]tgbotapi.InlineKeyboardButton
	for _, loan := range activeLoans {
		button := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("ID %d: %s - %d ₸", loan.ID, loan.Borrower, loan.Amount),
			fmt.Sprintf("edit_%d", loan.ID),
		)
		keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(button))
	}

	// Add back button
	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_manage"),
	))

	msg := tgbotapi.NewMessage(chatID, "Выберите займ для редактирования:")
	msg.ReplyMarkup = tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
	m.bot.Send(msg)

	// Set state for next step
	m.SetState(chatID, OpEditLoan, 0)
}

// StartDeleteLoanFlow begins the process of deleting a loan
func (m *BotManager) StartDeleteLoanFlow(chatID int64) {
	// First clear any existing state
	m.ClearState(chatID)

	// Show all loans to select from
	allLoans, err := m.GetAllLoansForUser(chatID)
	if err != nil {
		log.Printf("Error getting loans: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить список займов.")
		m.ShowMainMenu(chatID)
		return
	}

	if len(allLoans) == 0 {
		m.SendMessage(chatID, "У вас нет займов для удаления.")
		m.ShowMainMenu(chatID)
		return
	}

	// Display loans with inline keyboard for selection
	var keyboard [][]tgbotapi.InlineKeyboardButton
	for _, loan := range allLoans {
		status := "✅ возвращен"
		if !loan.Repaid {
			status = "⏳ активен"
		}

		button := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("ID %d: %s - %d ₸ (%s)", loan.ID, loan.Borrower, loan.Amount, status),
			fmt.Sprintf("delete_%d", loan.ID),
		)
		keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(button))
	}

	// Add back button
	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_manage"),
	))

	msg := tgbotapi.NewMessage(chatID, "⚠️ ВНИМАНИЕ! Вы собираетесь удалить займ:\n(Внимание: это действие нельзя отменить)")
	msg.ReplyMarkup = tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
	m.bot.Send(msg)

	// Set state for next step
	m.SetState(chatID, OpDeleteLoan, 0)
}

// StartPartialRepaymentFlow begins the process of recording a partial repayment
func (m *BotManager) StartPartialRepaymentFlow(chatID int64) {
	// First clear any existing state
	m.ClearState(chatID)

	// Show active loans to select from
	activeLoans, err := m.GetActiveLoansForUser(chatID)
	if err != nil {
		log.Printf("Error getting active loans: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить список активных займов.")
		m.ShowMainMenu(chatID)
		return
	}

	if len(activeLoans) == 0 {
		m.SendMessage(chatID, "У вас нет активных займов для частичного возврата.")
		m.ShowMainMenu(chatID)
		return
	}

	// Display loans with inline keyboard for selection
	var keyboard [][]tgbotapi.InlineKeyboardButton
	for _, loan := range activeLoans {
		remainingAmount := loan.Amount - m.GetTotalRepaidAmount(chatID, loan.ID)
		button := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("ID %d: %s - Осталось: %d ₸", loan.ID, loan.Borrower, remainingAmount),
			fmt.Sprintf("partial_%d", loan.ID),
		)
		keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(button))
	}

	// Add back button
	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_manage"),
	))

	msg := tgbotapi.NewMessage(chatID, "Выберите займ для частичного возврата:")
	msg.ReplyMarkup = tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
	m.bot.Send(msg)

	// Set state for next step
	m.SetState(chatID, OpPartialRepay, 0)
}

// ShowRepaymentHistory displays the repayment history for a user's loans
func (m *BotManager) ShowRepaymentHistory(chatID int64) {
	// Show all loans to select from
	allLoans, err := m.GetAllLoansForUser(chatID)
	if err != nil {
		log.Printf("Error getting loans: %v", err)
		m.SendMessage(chatID, "❌ Не удалось получить список займов.")
		m.ShowMainMenu(chatID)
		return
	}

	if len(allLoans) == 0 {
		m.SendMessage(chatID, "У вас нет займов с историей платежей.")
		m.ShowMainMenu(chatID)
		return
	}

	// Display loans with inline keyboard for selection
	var keyboard [][]tgbotapi.InlineKeyboardButton
	for _, loan := range allLoans {
		button := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("ID %d: %s - %d ₸", loan.ID, loan.Borrower, loan.Amount),
			fmt.Sprintf("history_%d", loan.ID),
		)
		keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(button))
	}

	// Add back button
	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "back_to_manage"),
	))

	msg := tgbotapi.NewMessage(chatID, "Выберите займ для просмотра истории платежей:")
	msg.ReplyMarkup = tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
	m.bot.Send(msg)
}
