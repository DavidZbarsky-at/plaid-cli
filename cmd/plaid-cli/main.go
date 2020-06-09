package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/landakram/plaid-cli/pkg/plaid_cli"
	"github.com/plaid/plaid-go/plaid"
	"github.com/spf13/cobra"

	"github.com/spf13/viper"
)

func main() {
	log.SetFlags(0)

	usr, _ := user.Current()
	dir := usr.HomeDir
	viper.SetDefault("cli.data_dir", filepath.Join(dir, ".plaid-cli"))

	dataDir := viper.GetString("cli.data_dir")
	data := plaid_cli.LoadData(dataDir)

	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.AddConfigPath(dataDir)
	viper.AddConfigPath(".")
	err := viper.ReadInConfig()
	if err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found; ignore error if desired
		} else {
			log.Fatal(err)
		}
	}

	viper.AutomaticEnv()

	opts := plaid.ClientOptions{
		viper.GetString("plaid.client_id"),
		viper.GetString("plaid.secret"),
		viper.GetString("plaid.public_key"),
		plaid.Development,
		&http.Client{},
	}

	client, err := plaid.NewClient(opts)

	if err != nil {
		log.Fatal(err)
	}

	linker := plaid_cli.NewLinker(data, client)

	linkCommand := &cobra.Command{
		Use:   "link [ITEM-ID-OR-ALIAS]",
		Short: "Link a bank account so plaid-cli can pull transactions.",
		Long:  "Link a bank account so plaid-cli can pull transactions. An item ID or alias can be passed to initiate a relink.",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			itemOrAlias := args[0]

			port := viper.GetString("link.port")

			var tokenPair *plaid_cli.TokenPair
			var err error

			if len(itemOrAlias) > 0 {
				itemID, ok := data.Aliases[itemOrAlias]
				if ok {
					itemOrAlias = itemID
				}

				tokenPair, err = linker.Relink(itemOrAlias, port)
			} else {
				tokenPair, err = linker.Link(port)
			}

			data.Tokens[tokenPair.ItemID] = tokenPair.AccessToken
			err = data.Save()
			if err != nil {
				log.Fatalln(err)
			}
		},
	}

	linkCommand.Flags().StringP("port", "p", "8080", "Port on which to serve Plaid Link")
	viper.BindPFlag("link.port", linkCommand.Flags().Lookup("port"))

	tokensCommand := &cobra.Command{
		Use:   "tokens",
		Short: "List tokens",
		Run: func(cmd *cobra.Command, args []string) {
			resolved := make(map[string]string)
			for itemID, token := range data.Tokens {
				if alias, ok := data.BackAliases[itemID]; ok {
					resolved[alias] = token
				} else {
					resolved[itemID] = token
				}
			}

			printJSON, err := json.MarshalIndent(resolved, "", "  ")
			if err != nil {
				log.Fatalln(err)
			}
			fmt.Println(string(printJSON))
		},
	}

	aliasCommand := &cobra.Command{
		Use:   "alias [ITEM-ID] [NAME]",
		Short: "Give a linked bank account a name.",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			itemID := args[0]
			name := args[1]

			if _, ok := data.Tokens[itemID]; !ok {
				log.Fatalf("No access token found for item ID `%s`. Try re-linking your account with `plaid-cli link`.\n", itemID)
			}

			data.Aliases[name] = itemID
			data.BackAliases[itemID] = name
			err = data.Save()
			if err != nil {
				log.Fatalln(err)
			}
		},
	}

	aliasesCommand := &cobra.Command{
		Use:   "aliases",
		Short: "List aliases",
		Run: func(cmd *cobra.Command, args []string) {
			printJSON, err := json.MarshalIndent(data.Aliases, "", "  ")
			if err != nil {
				log.Fatalln(err)
			}
			fmt.Println(string(printJSON))
		},
	}

	var fromFlag string
	var toFlag string
	var outputFormat string
	transactionsCommand := &cobra.Command{
		Use:   "transactions [ITEM-ID-OR-ALIAS]",
		Short: "List transactions for a given account",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			itemOrAlias := args[0]
			itemID, ok := data.Aliases[itemOrAlias]
			if ok {
				itemOrAlias = itemID
			}

			token := data.Tokens[itemOrAlias]
			res, err := client.GetTransactions(token, fromFlag, toFlag)
			if err != nil {
				log.Fatalln(err)
			}

			// TODO: Handle pagination

			serializer, err := NewTransactionSerializer(outputFormat)
			if err != nil {
				log.Fatalln(err)
			}

			b, err := serializer.serialize(res.Transactions)
			if err != nil {
				log.Fatalln(err)
			}
			fmt.Println(string(b))
		},
	}
	transactionsCommand.Flags().StringVarP(&fromFlag, "from", "f", "", "Date of first transaction (required)")
	transactionsCommand.MarkFlagRequired("from")

	transactionsCommand.Flags().StringVarP(&toFlag, "to", "t", "", "Date of last transaction (required)")
	transactionsCommand.MarkFlagRequired("to")

	transactionsCommand.Flags().StringVarP(&outputFormat, "output-format", "o", "json", "Output format")

	rootCommand := &cobra.Command{Use: "plaid-cli"}
	rootCommand.AddCommand(linkCommand)
	rootCommand.AddCommand(tokensCommand)
	rootCommand.AddCommand(aliasCommand)
	rootCommand.AddCommand(aliasesCommand)
	rootCommand.AddCommand(transactionsCommand)
	rootCommand.Execute()
}

type TransactionSerializer interface {
	serialize(txs []plaid.Transaction) ([]byte, error)
}

func NewTransactionSerializer(t string) (TransactionSerializer, error) {
	switch t {
	case "csv":
		return &CSVSerializer{}, nil
	case "json":
		return &JSONSerializer{}, nil
	default:
		return nil, errors.New(fmt.Sprintf("Invalid format: %s", t))
	}
}

type CSVSerializer struct{}

func (w *CSVSerializer) serialize(txs []plaid.Transaction) ([]byte, error) {
	var records [][]string
	for _, tx := range txs {
		sanitizedName := strings.ReplaceAll(tx.Name, ",", "")
		records = append(records, []string{tx.Date, fmt.Sprintf("%f", tx.Amount), sanitizedName})
	}

	b := bytes.NewBufferString("")
	writer := csv.NewWriter(b)
	err := writer.Write([]string{"Date", "Amount", "Description"})
	if err != nil {
		return nil, err
	}
	err = writer.WriteAll(records)
	if err != nil {
		return nil, err
	}

	return b.Bytes(), err
}

type JSONSerializer struct{}

func (w *JSONSerializer) serialize(txs []plaid.Transaction) ([]byte, error) {
	return json.MarshalIndent(txs, "", "  ")
}