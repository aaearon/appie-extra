package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appie "github.com/gwillem/appie-go"
)

// ---- Exit codes (shared CLI contract) ----

const (
	exitOK            = 0
	exitUserError     = 1
	exitAuthError     = 2
	exitUpstreamError = 3
	exitNotFound      = 4
)

// exitFn is swappable so tests can capture exit codes without terminating.
var exitFn = os.Exit

// ---- Global flags ----

type globalFlags struct {
	noImages bool
	verbose  bool
}

var flags globalFlags

// ---- Envelope types ----

type errorPayload struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type envelope struct {
	OK       bool           `json:"ok"`
	Data     any            `json:"data,omitempty"`
	Meta     map[string]any `json:"meta,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	Error    *errorPayload  `json:"error,omitempty"`
}

// ---- Emit helpers ----

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func emitSuccess(data any, meta map[string]any, warnings []string) {
	if err := writeJSON(os.Stdout, envelope{OK: true, Data: data, Meta: meta, Warnings: warnings}); err != nil {
		fmt.Fprintf(os.Stderr, "appie-extra: failed to encode output: %v\n", err)
		exitFn(exitUpstreamError)
	}
}

func emitError(code, message string, exitCode int) {
	emitErrorDetails(code, message, nil, exitCode)
}

func emitErrorDetails(code, message string, details map[string]any, exitCode int) {
	if err := writeJSON(os.Stdout, envelope{
		OK:    false,
		Error: &errorPayload{Code: code, Message: message, Details: details},
	}); err != nil {
		// Stdout unwritable — surface the error code+message on stderr so the
		// failure isn't completely silent.
		fmt.Fprintf(os.Stderr, "appie-extra: %s: %s\n", code, message)
	}
	exitFn(exitCode)
}

func emitPartial(data any, warnings []string, code, message string) {
	if err := writeJSON(os.Stdout, envelope{
		OK:       false,
		Data:     data,
		Warnings: warnings,
		Error:    &errorPayload{Code: code, Message: message},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "appie-extra: %s: %s\n", code, message)
	}
	exitFn(exitUserError)
}

// ---- Argument validation ----

func parsePositiveInt(arg, name string) int {
	v, err := strconv.Atoi(arg)
	if err != nil {
		emitError("invalid_int", fmt.Sprintf("%s must be an integer, got %q", name, arg), exitUserError)
		return 0
	}
	if v <= 0 {
		emitError("invalid_int", fmt.Sprintf("%s must be positive, got %d", name, v), exitUserError)
		return 0
	}
	return v
}

func parseNonNegativeInt(arg, name string) int {
	v, err := strconv.Atoi(arg)
	if err != nil {
		emitError("invalid_int", fmt.Sprintf("%s must be an integer, got %q", name, arg), exitUserError)
		return 0
	}
	if v < 0 {
		emitError("invalid_int", fmt.Sprintf("%s must be >= 0, got %d", name, v), exitUserError)
		return 0
	}
	return v
}

func clampMax(v, max int) int {
	if v > max {
		return max
	}
	return v
}

func requireExactArgs(args []string, n int, usage string) {
	if len(args) != n {
		emitError("bad_args", fmt.Sprintf("expected %d argument(s); usage: %s", n, usage), exitUserError)
	}
}

func requireAtMostArgs(args []string, n int, usage string) {
	if len(args) > n {
		emitError("unexpected_arg", fmt.Sprintf("expected at most %d argument(s); usage: %s", n, usage), exitUserError)
	}
}

func requireAtLeastArgs(args []string, n int, usage string) {
	if len(args) < n {
		emitError("missing_arg", fmt.Sprintf("expected at least %d argument(s); usage: %s", n, usage), exitUserError)
	}
}

// ---- Global flag pre-pass ----

func parseGlobalFlags(raw []string) (globalFlags, []string) {
	var gf globalFlags
	rest := make([]string, 0, len(raw))
	for _, a := range raw {
		switch a {
		case "--no-images", "--minimal":
			gf.noImages = true
		case "--verbose":
			gf.verbose = true
		case "--help", "-h":
			// Let dispatch route help to cmdHelp; don't reject it here.
			rest = append(rest, a)
		default:
			if strings.HasPrefix(a, "--") {
				emitError("bad_args", fmt.Sprintf("unknown flag: %s", a), exitUserError)
				return gf, rest
			}
			rest = append(rest, a)
		}
	}
	return gf, rest
}

// ---- Image stripping ----

// stripImagesFromProducts strips the "images" key from each product. On
// marshal/unmarshal failure the original product is preserved so the result
// always has the same count as the input.
func stripImagesFromProducts(products []appie.Product) []any {
	out := make([]any, 0, len(products))
	for i, p := range products {
		b, err := json.Marshal(p)
		if err != nil {
			out = append(out, products[i])
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			out = append(out, products[i])
			continue
		}
		delete(m, "images")
		out = append(out, m)
	}
	return out
}

func stripImagesFromRaw(b json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	stripImagesInPlace(v)
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

func stripImagesInPlace(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "images")
		for _, val := range t {
			stripImagesInPlace(val)
		}
	case []any:
		for _, item := range t {
			stripImagesInPlace(item)
		}
	}
}

// decodeRawToAny decodes a json.RawMessage to a generic any; applies image
// stripping when --no-images is set. Returns nil for null/empty input.
func decodeRawToAny(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	if flags.noImages {
		stripImagesInPlace(v)
	}
	return v
}

// ---- Auth + config ----

func configPath() string {
	if p := os.Getenv("APPIE_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "appie", "config.json")
}

func mustAuthOrEmit(ctx context.Context) *appie.Client {
	cp := configPath()
	client, err := appie.NewWithConfig(cp)
	if err != nil {
		emitError("not_authenticated", fmt.Sprintf("not authenticated; run 'appie login' (%v)", err), exitAuthError)
		return nil
	}
	if !client.IsAuthenticated() {
		emitError("not_authenticated", "not authenticated; run 'appie login'", exitAuthError)
		return nil
	}
	return client
}

func doGraphQL(ctx context.Context, client *appie.Client, query string, result any) error {
	return client.DoGraphQL(ctx, query, nil, result)
}

// ---- Commands ----

func cmdMember(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra member")
	client := mustAuthOrEmit(ctx)
	member, err := client.GetMember(ctx)
	if err != nil {
		emitError("upstream_failed", fmt.Sprintf("GetMember failed: %v", err), exitUpstreamError)
	}
	emitSuccess(member, nil, nil)
}

func cmdPreviouslyBought(ctx context.Context, args []string) {
	requireAtMostArgs(args, 2, "appie-extra previously-bought [size] [page]")
	client := mustAuthOrEmit(ctx)

	size := 50
	page := 0
	if len(args) > 0 {
		size = parsePositiveInt(args[0], "size")
	}
	if len(args) > 1 {
		page = parseNonNegativeInt(args[1], "page")
	}

	query := fmt.Sprintf(`{
		productSearch(input: {
			query: ""
			previouslyBought: true
			size: %d
			page: %d
		}) {
			products {
				id
				title
				brand
				category
			}
			page {
				totalElements
				totalPages
			}
		}
	}`, size, page)

	var result struct {
		ProductSearch struct {
			Products []json.RawMessage `json:"products"`
			Page     struct {
				TotalElements int `json:"totalElements"`
				TotalPages    int `json:"totalPages"`
			} `json:"page"`
		} `json:"productSearch"`
	}

	if err := doGraphQL(ctx, client, query, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("previously-bought query failed: %v", err), exitUpstreamError)
	}

	products := make([]any, 0, len(result.ProductSearch.Products))
	for _, p := range result.ProductSearch.Products {
		products = append(products, decodeRawToAny(p))
	}

	emitSuccess(products, map[string]any{
		"total": result.ProductSearch.Page.TotalElements,
		"pages": result.ProductSearch.Page.TotalPages,
		"page":  page,
		"size":  size,
	}, nil)
}

func cmdSearchRecipes(ctx context.Context, args []string) {
	requireAtMostArgs(args, 2, "appie-extra search-recipes [query] [size]")
	client := mustAuthOrEmit(ctx)

	queryText := ""
	size := 10
	if len(args) > 0 {
		queryText = args[0]
	}
	if len(args) > 1 {
		size = parsePositiveInt(args[1], "size")
	}
	if size > 100 {
		size = 100
	}

	sanitizedQuery := strings.ReplaceAll(queryText, `"`, ``)
	sanitizedQuery = strings.ReplaceAll(sanitizedQuery, `\`, ``)
	sanitizedQuery = strings.ReplaceAll(sanitizedQuery, "\n", " ")

	var query string
	if sanitizedQuery != "" {
		query = fmt.Sprintf(`{ recipeSearch(query: { ingredients: "%s", size: %d }) { result { id title slug } } }`, sanitizedQuery, size)
	} else {
		query = fmt.Sprintf(`{ recipeSearch(query: { size: %d }) { result { id title slug } } }`, size)
	}

	var result struct {
		RecipeSearch struct {
			Result []json.RawMessage `json:"result"`
		} `json:"recipeSearch"`
	}

	if err := client.DoGraphQL(ctx, query, nil, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("recipe search failed: %v", err), exitUpstreamError)
	}

	recipes := make([]any, 0, len(result.RecipeSearch.Result))
	for _, r := range result.RecipeSearch.Result {
		recipes = append(recipes, decodeRawToAny(r))
	}

	emitSuccess(recipes, map[string]any{"total": len(recipes)}, nil)
}

func cmdRecipe(ctx context.Context, args []string) {
	requireExactArgs(args, 1, "appie-extra recipe <id>")
	client := mustAuthOrEmit(ctx)

	id := parsePositiveInt(args[0], "id")

	query := fmt.Sprintf(`{
		recipe(id: %d) {
			id
			title
			description
			cookTime
			servings { number }
			ingredients {
				text
				quantity
				name { singular plural }
			}
		}
	}`, id)

	var result struct {
		Recipe json.RawMessage `json:"recipe"`
	}

	if err := doGraphQL(ctx, client, query, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("get recipe failed: %v", err), exitUpstreamError)
	}

	decoded := decodeRawToAny(result.Recipe)
	if decoded == nil {
		emitError("not_found", fmt.Sprintf("recipe %d not found", id), exitNotFound)
	}

	emitSuccess(decoded, nil, nil)
}

func cmdBonusProducts(ctx context.Context, args []string) {
	requireAtMostArgs(args, 1, "appie-extra bonus-products [limit]")
	client := mustAuthOrEmit(ctx)

	products, err := client.GetBonusProducts(ctx)
	if err != nil {
		emitError("upstream_failed", fmt.Sprintf("GetBonusProducts failed: %v", err), exitUpstreamError)
	}

	limit := 50
	if len(args) > 0 {
		limit = parsePositiveInt(args[0], "limit")
	}
	limit = clampMax(limit, len(products))

	sliced := products[:limit]
	var data any
	if flags.noImages {
		data = stripImagesFromProducts(sliced)
	} else {
		data = sliced
	}

	emitSuccess(data, map[string]any{
		"total": len(products),
		"limit": limit,
	}, nil)
}

func cmdBonusSpotlight(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra bonus-spotlight")
	client := mustAuthOrEmit(ctx)

	products, err := client.GetSpotlightBonusProducts(ctx)
	if err != nil {
		emitError("upstream_failed", fmt.Sprintf("GetSpotlightBonusProducts failed: %v", err), exitUpstreamError)
	}

	var data any
	if flags.noImages {
		data = stripImagesFromProducts(products)
	} else {
		data = products
	}
	emitSuccess(data, map[string]any{"total": len(products)}, nil)
}

func cmdBonusBox(ctx context.Context, args []string) {
	requireAtMostArgs(args, 1, "appie-extra bonusbox [next|YYYY-MM-DD]")
	client := mustAuthOrEmit(ctx)

	var bonusDate string
	if len(args) > 0 {
		if args[0] == "next" {
			now := time.Now()
			daysSinceSun := int(now.Weekday())
			bonusDate = now.AddDate(0, 0, -daysSinceSun+7).Format("2006-01-02")
		} else {
			bonusDate = args[0]
		}
	} else {
		now := time.Now()
		daysSinceSun := int(now.Weekday())
		bonusDate = now.AddDate(0, 0, -daysSinceSun).Format("2006-01-02")
	}

	var result json.RawMessage
	ep := "/mobile-services/bonuspage/v1/personal?bonusStartDate=" + bonusDate
	if err := client.DoRequest(ctx, "GET", ep, nil, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("bonusbox request failed (date %s): %v", bonusDate, err), exitUpstreamError)
	}

	emitSuccess(decodeRawToAny(result), map[string]any{"bonusStartDate": bonusDate}, nil)
}

func cmdAddFreetext(ctx context.Context, args []string) {
	requireAtLeastArgs(args, 1, "appie-extra add-freetext <text> [quantity]")
	requireAtMostArgs(args, 2, "appie-extra add-freetext <text> [quantity]")
	client := mustAuthOrEmit(ctx)

	text := args[0]
	qty := 1
	if len(args) > 1 {
		qty = parsePositiveInt(args[1], "quantity")
	}
	if err := client.AddFreeTextToShoppingList(ctx, text, qty); err != nil {
		emitError("upstream_failed", fmt.Sprintf("AddFreeTextToShoppingList failed: %v", err), exitUpstreamError)
	}

	emitSuccess(map[string]any{
		"text":     text,
		"quantity": qty,
		"action":   "added",
	}, nil, nil)
}

func cmdBatchAdd(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra batch-add (reads JSON from stdin)")
	client := mustAuthOrEmit(ctx)

	var items []struct {
		ID   int    `json:"id"`
		Qty  int    `json:"qty"`
		Text string `json:"text"`
	}

	if err := json.NewDecoder(os.Stdin).Decode(&items); err != nil {
		emitError("invalid_input", fmt.Sprintf("invalid JSON input: %v", err), exitUserError)
	}

	added := 0
	failures := make([]map[string]any, 0)
	warnings := make([]string, 0)

	for i, item := range items {
		switch {
		case item.Text != "":
			q := item.Qty
			if q <= 0 {
				q = 1
			}
			if err := client.AddFreeTextToShoppingList(ctx, item.Text, q); err != nil {
				failures = append(failures, map[string]any{
					"index": i,
					"text":  item.Text,
					"error": err.Error(),
				})
				warnings = append(warnings, fmt.Sprintf("failed to add freetext %q: %v", item.Text, err))
				continue
			}
			added++
		case item.ID > 0:
			q := item.Qty
			if q <= 0 {
				q = 1
			}
			if err := client.AddProductToShoppingList(ctx, item.ID, q); err != nil {
				failures = append(failures, map[string]any{
					"index": i,
					"id":    item.ID,
					"error": err.Error(),
				})
				warnings = append(warnings, fmt.Sprintf("failed to add product %d: %v", item.ID, err))
				continue
			}
			added++
		default:
			failures = append(failures, map[string]any{
				"index": i,
				"error": "item has neither text nor id",
			})
			warnings = append(warnings, fmt.Sprintf("item %d has neither text nor id", i))
		}
	}

	data := map[string]any{
		"added":    added,
		"total":    len(items),
		"failures": failures,
	}

	if len(failures) > 0 {
		emitPartial(data, warnings,
			"partial_failure",
			fmt.Sprintf("%d of %d items failed", len(failures), len(items)))
		return
	}

	emitSuccess(data, nil, nil)
}

func cmdKoopzegels(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra koopzegels")
	client := mustAuthOrEmit(ctx)

	query := `{
		purchaseStampBalance {
			points { currentBookletPoints fullBooklets totalPoints }
			money { invested { amount } interest { amount } payout { amount } }
			constants {
				price { amount }
				partialBookletTarget { points interest { amount } }
				fullBookletTarget { points interest { amount } }
			}
		}
		purchaseStampSavingGoal { target: amount { amount } name }
	}`

	var result json.RawMessage
	if err := doGraphQL(ctx, client, query, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("koopzegels query failed: %v", err), exitUpstreamError)
	}

	emitSuccess(decodeRawToAny(result), nil, nil)
}

func cmdBrabantia(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra brabantia")
	client := mustAuthOrEmit(ctx)

	programQuery := `query FetchLoyaltyProgram($programId: Int!) {
		loyaltyProgram(programId: $programId) {
			id name type status
			savingPeriod { start end }
			redeemPeriod { start end }
			content { title }
		}
	}`

	var programResult json.RawMessage
	if err := client.DoGraphQL(ctx, programQuery, map[string]any{"programId": 217, "withProducts": false}, &programResult); err != nil {
		emitError("upstream_failed", fmt.Sprintf("brabantia program query failed: %v", err), exitUpstreamError)
	}

	balanceQuery := `query FetchLoyaltyPointsBalance($programIds: [Int!]!) {
		loyaltyPointsBalances(programIds: $programIds) { programId balance }
	}`

	var balanceResult json.RawMessage
	if err := client.DoGraphQL(ctx, balanceQuery, map[string]any{"programIds": []int{217}}, &balanceResult); err != nil {
		emitError("upstream_failed", fmt.Sprintf("brabantia balance query failed: %v", err), exitUpstreamError)
	}

	emitSuccess(map[string]any{
		"program": decodeRawToAny(programResult),
		"balance": decodeRawToAny(balanceResult),
	}, nil, nil)
}

func loadDeliveryAddress() map[string]any {
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, "grocery-assistant", "ah", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		emitError("config_error", fmt.Sprintf("cannot read %s: %v", cfgPath, err), exitUserError)
		return nil
	}
	var cfg struct {
		DeliveryAddress *struct {
			City        string `json:"city"`
			CountryCode string `json:"country_code"`
			HouseNumber int    `json:"house_number"`
			PostalCode  string `json:"postal_code"`
			Street      string `json:"street"`
		} `json:"delivery_address"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		emitError("config_error", fmt.Sprintf("cannot parse %s: %v", cfgPath, err), exitUserError)
		return nil
	}
	if cfg.DeliveryAddress == nil {
		emitError("config_error",
			fmt.Sprintf("no delivery_address in %s (need: city, country_code, house_number, postal_code, street)", cfgPath),
			exitUserError)
		return nil
	}
	return map[string]any{
		"city":        cfg.DeliveryAddress.City,
		"countryCode": cfg.DeliveryAddress.CountryCode,
		"houseNumber": cfg.DeliveryAddress.HouseNumber,
		"postalCode":  cfg.DeliveryAddress.PostalCode,
		"street":      cfg.DeliveryAddress.Street,
	}
}

func cmdDeliverySlots(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra delivery-slots")
	client := mustAuthOrEmit(ctx)

	query := `query FetchDeliveryOrderSlotDays($address: MemberAddressInput!) {
		orderDeliverySlots(address: $address) {
			date isFullyBooked
			slots {
				startTime endTime
				deliveryLocationId shiftCode
				isFullyBooked
				serviceCharge { defaultPrice { amount } price { amount } }
				nudgeType
			}
		}
	}`

	vars := map[string]any{"address": loadDeliveryAddress()}

	var result json.RawMessage
	if err := client.DoGraphQL(ctx, query, vars, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("delivery-slots query failed: %v", err), exitUpstreamError)
	}

	emitSuccess(decodeRawToAny(result), nil, nil)
}

func cmdBasket(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra basket")
	client := mustAuthOrEmit(ctx)

	query := `{ basket { canChangeDelivery itemsInOrder { product { id title salesUnitSize } quantity } itemsInList { product { id title salesUnitSize } quantity } } }`

	var result json.RawMessage
	if err := doGraphQL(ctx, client, query, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("basket query failed: %v", err), exitUpstreamError)
	}

	emitSuccess(decodeRawToAny(result), nil, nil)
}

func cmdBasketAdd(ctx context.Context, args []string) {
	requireAtLeastArgs(args, 1, "appie-extra basket-add <product-id> [quantity]")
	requireAtMostArgs(args, 2, "appie-extra basket-add <product-id> [quantity]")
	client := mustAuthOrEmit(ctx)

	productID := parsePositiveInt(args[0], "product-id")
	qty := 1
	if len(args) > 1 {
		qty = parsePositiveInt(args[1], "quantity")
	}

	mutation := `mutation UpdateMyListBasket($items: [BasketMutation!]!, $input: BasketInput) { basketItemsUpdate(items: $items, input: $input) { status } }`
	vars := map[string]any{
		"input": nil,
		"items": []map[string]any{
			{"id": productID, "isStrikethrough": false, "quantity": qty},
		},
	}

	var result json.RawMessage
	if err := client.DoGraphQL(ctx, mutation, vars, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("basket add failed: %v", err), exitUpstreamError)
	}

	emitSuccess(map[string]any{
		"productId": productID,
		"quantity":  qty,
		"action":    "added",
	}, nil, nil)
}

func cmdBasketRemove(ctx context.Context, args []string) {
	requireExactArgs(args, 1, "appie-extra basket-remove <product-id>")
	client := mustAuthOrEmit(ctx)

	productID := parsePositiveInt(args[0], "product-id")

	mutation := `mutation UpdateMyListBasket($items: [BasketMutation!]!, $input: BasketInput) { basketItemsUpdate(items: $items, input: $input) { status } }`
	vars := map[string]any{
		"input": nil,
		"items": []map[string]any{
			{"id": productID, "isStrikethrough": false, "quantity": 0},
		},
	}

	var result json.RawMessage
	if err := client.DoGraphQL(ctx, mutation, vars, &result); err != nil {
		emitError("upstream_failed", fmt.Sprintf("basket remove failed: %v", err), exitUpstreamError)
	}

	emitSuccess(map[string]any{
		"productId": productID,
		"action":    "removed",
	}, nil, nil)
}

func cmdListToOrder(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra list-to-order")
	client := mustAuthOrEmit(ctx)

	if err := client.ShoppingListToOrder(ctx); err != nil {
		emitError("upstream_failed", fmt.Sprintf("ShoppingListToOrder failed: %v", err), exitUpstreamError)
	}

	emitSuccess(map[string]any{"action": "converted"}, nil, nil)
}

func cmdOrderSummary(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra order-summary")
	client := mustAuthOrEmit(ctx)

	summary, err := client.GetOrderSummary(ctx)
	if err != nil {
		emitError("upstream_failed", fmt.Sprintf("GetOrderSummary failed: %v", err), exitUpstreamError)
	}

	emitSuccess(summary, nil, nil)
}

func cmdClearOrder(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra clear-order")
	client := mustAuthOrEmit(ctx)

	if err := client.ClearOrder(ctx); err != nil {
		emitError("upstream_failed", fmt.Sprintf("ClearOrder failed: %v", err), exitUpstreamError)
	}

	emitSuccess(map[string]any{"action": "cleared"}, nil, nil)
}

func cmdFulfillments(ctx context.Context, args []string) {
	requireExactArgs(args, 0, "appie-extra fulfillments")
	client := mustAuthOrEmit(ctx)

	fulfillments, err := client.GetFulfillments(ctx)
	if err != nil {
		emitError("upstream_failed", fmt.Sprintf("GetFulfillments failed: %v", err), exitUpstreamError)
	}

	emitSuccess(fulfillments, map[string]any{"total": len(fulfillments)}, nil)
}

// ---- Help ----

type commandDoc struct {
	Name        string `json:"name"`
	Usage       string `json:"usage"`
	Description string `json:"description"`
}

func cmdHelp() {
	cmds := []commandDoc{
		{"member", "member", "Show member profile and segmentation data"},
		{"previously-bought", "previously-bought [size] [page]", "Get purchase history (default size=50, page=0)"},
		{"search-recipes", "search-recipes [query] [size]", "Search Allerhande recipes (default size=10)"},
		{"recipe", "recipe <id>", "Get full recipe with ingredients and steps"},
		{"bonus-products", "bonus-products [limit]", "Get all current bonus products (default 50)"},
		{"bonus-spotlight", "bonus-spotlight", "Get featured/highlighted bonus products"},
		{"bonusbox", "bonusbox [next|YYYY-MM-DD]", "Show personal Bonus Box offers (default: this week)"},
		{"add-freetext", "add-freetext <text> [qty]", "Add free-text item to shopping list"},
		{"batch-add", "batch-add (stdin JSON)", "Add multiple items from stdin JSON"},
		{"list-to-order", "list-to-order", "Convert shopping list to active order"},
		{"order-summary", "order-summary", "Show order pricing totals"},
		{"clear-order", "clear-order", "Empty the active order"},
		{"fulfillments", "fulfillments", "Show scheduled deliveries"},
		{"koopzegels", "koopzegels", "Show koopzegels balance and savings"},
		{"brabantia", "brabantia", "Show Brabantia spaaractie status and balance"},
		{"delivery-slots", "delivery-slots", "Show available delivery time slots"},
		{"basket", "basket", "Show current winkelmandje contents"},
		{"basket-add", "basket-add <product-id> [qty]", "Add product to winkelmandje"},
		{"basket-remove", "basket-remove <product-id>", "Remove product from winkelmandje"},
		{"help", "help", "Show this command list"},
	}

	emitSuccess(map[string]any{
		"commands": cmds,
		"flags": []map[string]string{
			{"name": "--no-images", "description": "Strip image URLs from product payloads (aka --minimal)"},
			{"name": "--verbose", "description": "Increase logging verbosity"},
		},
		"exit_codes": map[string]int{
			"success":          exitOK,
			"user_error":       exitUserError,
			"auth_error":       exitAuthError,
			"upstream_error":   exitUpstreamError,
			"not_found":        exitNotFound,
		},
		"config_path": configPath(),
	}, nil, nil)
}

// ---- Dispatch ----

func main() {
	if len(os.Args) < 2 {
		emitError("missing_arg", "no command provided; try 'appie-extra help'", exitUserError)
		return
	}

	ctx := context.Background()
	gf, rest := parseGlobalFlags(os.Args[1:])
	flags = gf
	if len(rest) == 0 {
		emitError("missing_arg", "no command provided; try 'appie-extra help'", exitUserError)
		return
	}
	cmd := rest[0]
	args := rest[1:]

	switch cmd {
	case "member":
		cmdMember(ctx, args)
	case "previously-bought":
		cmdPreviouslyBought(ctx, args)
	case "search-recipes":
		cmdSearchRecipes(ctx, args)
	case "recipe":
		cmdRecipe(ctx, args)
	case "bonus-products":
		cmdBonusProducts(ctx, args)
	case "bonus-spotlight":
		cmdBonusSpotlight(ctx, args)
	case "bonusbox", "bonus-box":
		cmdBonusBox(ctx, args)
	case "add-freetext":
		cmdAddFreetext(ctx, args)
	case "batch-add":
		cmdBatchAdd(ctx, args)
	case "list-to-order":
		cmdListToOrder(ctx, args)
	case "order-summary":
		cmdOrderSummary(ctx, args)
	case "clear-order":
		cmdClearOrder(ctx, args)
	case "fulfillments":
		cmdFulfillments(ctx, args)
	case "koopzegels", "stamps":
		cmdKoopzegels(ctx, args)
	case "brabantia":
		cmdBrabantia(ctx, args)
	case "delivery-slots", "slots":
		cmdDeliverySlots(ctx, args)
	case "basket":
		cmdBasket(ctx, args)
	case "basket-add":
		cmdBasketAdd(ctx, args)
	case "basket-remove":
		cmdBasketRemove(ctx, args)
	case "--help", "-h", "help":
		cmdHelp()
	default:
		emitError("unknown_command", fmt.Sprintf("unknown command: %s (try 'appie-extra help')", cmd), exitUserError)
	}
}
