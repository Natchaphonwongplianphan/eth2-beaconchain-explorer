package handlers

import (
	"encoding/hex"
	"encoding/json"
	"eth2-exporter/db"
	"eth2-exporter/price"
	"eth2-exporter/services"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gorilla/sessions"
	"github.com/lib/pq"
)

var pkeyRegex = regexp.MustCompile("[^0-9A-Fa-f]+")

// GetValidatorEarnings will return the earnings (last day, week, month and total) of selected validators
func GetValidatorEarnings(validators []uint64, currency string) (*types.ValidatorEarnings, error) {
	validatorsPQArray := pq.Array(validators)
	latestEpoch := int64(services.LatestEpoch())
	lastDayEpoch := latestEpoch - 225
	lastWeekEpoch := latestEpoch - 225*7
	lastMonthEpoch := latestEpoch - 225*31

	if lastDayEpoch < 0 {
		lastDayEpoch = 0
	}
	if lastWeekEpoch < 0 {
		lastWeekEpoch = 0
	}
	if lastMonthEpoch < 0 {
		lastMonthEpoch = 0
	}

	balances := []*types.Validator{}

	err := db.ReaderDb.Select(&balances, `SELECT 
			   COALESCE(balance, 0) AS balance, 
			   COALESCE(balanceactivation, 0) AS balanceactivation, 
			   COALESCE(balance1d, 0) AS balance1d, 
			   COALESCE(balance7d, 0) AS balance7d, 
			   COALESCE(balance31d , 0) AS balance31d,
       			activationepoch,
       			pubkey
		FROM validators WHERE validatorindex = ANY($1)`, validatorsPQArray)
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	deposits := []struct {
		Epoch     int64
		Amount    int64
		Publickey []byte
	}{}

	err = db.ReaderDb.Select(&deposits, "SELECT block_slot / 32 AS epoch, amount, publickey FROM blocks_deposits WHERE publickey IN (SELECT pubkey FROM validators WHERE validatorindex = ANY($1))", validatorsPQArray)
	if err != nil {
		return nil, err
	}

	depositsMap := make(map[string]map[int64]int64)
	for _, d := range deposits {
		if _, exists := depositsMap[fmt.Sprintf("%x", d.Publickey)]; !exists {
			depositsMap[fmt.Sprintf("%x", d.Publickey)] = make(map[int64]int64)
		}
		depositsMap[fmt.Sprintf("%x", d.Publickey)][d.Epoch] += d.Amount
	}

	var earningsTotal int64
	var earningsLastDay int64
	var earningsLastWeek int64
	var earningsLastMonth int64
	var apr float64
	var totalDeposits int64

	for _, balance := range balances {
		if int64(balance.ActivationEpoch) >= latestEpoch {
			continue
		}
		for epoch, deposit := range depositsMap[fmt.Sprintf("%x", balance.PublicKey)] {
			totalDeposits += deposit

			if epoch > int64(balance.ActivationEpoch) {
				earningsTotal -= deposit
			}
			if epoch > lastDayEpoch && epoch > int64(balance.ActivationEpoch) {
				earningsLastDay -= deposit
			}
			if epoch > lastWeekEpoch && epoch > int64(balance.ActivationEpoch) {
				earningsLastWeek -= deposit
			}
			if epoch > lastMonthEpoch && epoch > int64(balance.ActivationEpoch) {
				earningsLastMonth -= deposit
			}
		}

		if int64(balance.ActivationEpoch) > lastDayEpoch {
			balance.Balance1d = balance.BalanceActivation
		}
		if int64(balance.ActivationEpoch) > lastWeekEpoch {
			balance.Balance7d = balance.BalanceActivation
		}
		if int64(balance.ActivationEpoch) > lastMonthEpoch {
			balance.Balance31d = balance.BalanceActivation
		}

		earningsTotal += int64(balance.Balance) - balance.BalanceActivation.Int64
		earningsLastDay += int64(balance.Balance) - balance.Balance1d.Int64
		earningsLastWeek += int64(balance.Balance) - balance.Balance7d.Int64
		earningsLastMonth += int64(balance.Balance) - balance.Balance31d.Int64
	}

	if totalDeposits == 0 {
		totalDeposits = 32 * 1e9
	}

	apr = (((float64(earningsLastWeek) / 1e9) / (float64(totalDeposits) / 1e9)) * 365) / 7
	if apr < float64(-1) {
		apr = float64(-1)
	}

	return &types.ValidatorEarnings{
		Total:                earningsTotal,
		LastDay:              earningsLastDay,
		LastWeek:             earningsLastWeek,
		LastMonth:            earningsLastMonth,
		APR:                  apr,
		TotalDeposits:        totalDeposits,
		LastDayFormatted:     utils.FormatIncome(earningsLastDay, currency),
		LastWeekFormatted:    utils.FormatIncome(earningsLastWeek, currency),
		LastMonthFormatted:   utils.FormatIncome(earningsLastMonth, currency),
		TotalFormatted:       utils.FormatIncome(earningsTotal, currency),
		TotalChangeFormatted: utils.FormatIncome(earningsTotal+totalDeposits, currency),
	}, nil
}

// LatestState will return common information that about the current state of the eth2 chain
func LatestState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	currency := GetCurrency(r)
	data := services.LatestState()
	// data.Currency = currency
	data.EthPrice = price.GetEthPrice(currency)
	data.EthRoundPrice = price.GetEthRoundPrice(data.EthPrice)
	data.EthTruncPrice = utils.KFormatterEthPrice(data.EthRoundPrice)

	err := json.NewEncoder(w).Encode(data)

	if err != nil {
		logger.Errorf("error sending latest index page data: %v", err)
		http.Error(w, "Internal server error", http.StatusServiceUnavailable)
		return
	}
}

func GetCurrency(r *http.Request) string {
	if cookie, err := r.Cookie("currency"); err == nil {
		return cookie.Value
	}

	return "ETH"
}

func GetCurrencySymbol(r *http.Request) string {

	cookie, err := r.Cookie("currency")
	if err != nil {
		return "$"
	}

	switch cookie.Value {
	case "AUD":
		return "A$"
	case "CAD":
		return "C$"
	case "CNY":
		return "¥"
	case "EUR":
		return "€"
	case "GBP":
		return "£"
	case "JPY":
		return "¥"
	case "RUB":
		return "₽"
	default:
		return "$"
	}
}

func GetCurrentPrice(r *http.Request) uint64 {
	cookie, err := r.Cookie("currency")
	if err != nil {
		return price.GetEthRoundPrice(price.GetEthPrice("USD"))
	}

	if cookie.Value == "ETH" {
		return price.GetEthRoundPrice(price.GetEthPrice("USD"))
	}
	return price.GetEthRoundPrice(price.GetEthPrice(cookie.Value))
}

func GetCurrentPriceFormatted(r *http.Request) template.HTML {
	userAgent := r.Header.Get("User-Agent")
	userAgent = strings.ToLower(userAgent)
	price := GetCurrentPrice(r)
	if strings.Contains(userAgent, "android") || strings.Contains(userAgent, "iphone") || strings.Contains(userAgent, "windows phone") {
		return utils.KFormatterEthPrice(price)
	}
	return utils.FormatAddCommas(uint64(price))
}

func GetTruncCurrentPriceFormatted(r *http.Request) string {
	price := GetCurrentPrice(r)
	symbol := GetCurrencySymbol(r)
	return fmt.Sprintf("%s %s", symbol, utils.KFormatterEthPrice(price))
}

// GetValidatorIndexFrom gets the validator index from users input
func GetValidatorIndexFrom(userInput string) (pubKey []byte, validatorIndex uint64, err error) {
	validatorIndex, err = strconv.ParseUint(userInput, 10, 64)
	if err == nil {
		pubKey, err = db.GetValidatorPublicKey(validatorIndex)
		return
	}

	pubKey, err = hex.DecodeString(strings.Replace(userInput, "0x", "", -1))
	if err == nil {
		validatorIndex, err = db.GetValidatorIndex(pubKey)
		return
	}
	return
}

func DataTableStateChanges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	user, session, err := getUserSession(r)
	if err != nil {
		logger.Errorf("error retrieving session: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	response := &types.ApiResponse{}
	response.Status = "ERROR"

	defer json.NewEncoder(w).Encode(response)

	settings := types.DataTableSaveState{}
	err = json.NewDecoder(r.Body).Decode(&settings)
	if err != nil {
		logger.Errorf("error saving data table state could not parse body: %v", err)
		response.Status = "error saving table state"
		return
	}

	key := settings.Key
	if len(key) == 0 {
		logger.Errorf("no key provided")
		response.Status = "error saving table state"
		return
	}

	if !user.Authenticated {
		dataTableStatePrefix := "table:state:" + utils.GetNetwork() + ":"
		key = dataTableStatePrefix + key
		count := 0
		for k := range session.Values {
			k, ok := k.(string)
			if ok && strings.HasPrefix(k, dataTableStatePrefix) {
				count += 1
			}
		}
		if count > 50 {
			_, ok := session.Values[key]
			if !ok {
				logger.Errorf("error maximum number of datatable states stored in session")
				return
			}
		}
		session.Values[key] = settings

		err := session.Save(r, w)
		if err != nil {
			logger.WithError(err).Errorf("error updating session with key: %v and value: %v", key, settings)
		}

	} else {
		err = db.SaveDataTableState(user.UserID, settings.Key, settings)
		if err != nil {
			logger.Errorf("error saving data table state could save values to db: %v", err)
			response.Status = "error saving table state"
			return
		}
	}

	response.Status = "OK"
	response.Data = ""
}

func GetDataTableState(user *types.User, session *sessions.Session, tableKey string) (*types.DataTableSaveState, error) {
	if user.Authenticated {
		state, err := db.GetDataTablesState(user.UserID, tableKey)
		if err != nil {
			return nil, err
		}
		return state, nil
	}
	stateRaw, exists := session.Values["table:state:"+utils.GetNetwork()+":"+tableKey]
	if !exists {
		return nil, nil
	}
	state, ok := stateRaw.(types.DataTableSaveState)
	if !ok {
		return nil, fmt.Errorf("error parsing session value into type DataTableSaveState")
	}
	return &state, nil
}
