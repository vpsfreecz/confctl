package core

import (
	"context"
	"errors"

	"github.com/vpsfreecz/confctl/internal/gendata"
)

type genDataLeaf uint8

const (
	genDataContainers genDataLeaf = iota + 1
	genDataNetwork
	genDataAll
)

type genDataBindings struct {
	NewClient func(gendata.Prompt, func(string)) (*gendata.VPSAdmin, error)
	Prompt    gendata.Prompt
	Warn      func(string)
	Machines  func(context.Context) ([]Machine, error)
	Literal   func(any) (string, error)
}

// runGenData borrows one invocation's callbacks and ordered raw inventory.
// Authentication precedes inventory; network never reaches it. The actual
// provider, Generator and formatter retain reached cancellation/file effects.
// Public dispatch, native prompting and warning writer errors belong to callers.
func runGenData(ctx context.Context, root string, leaf genDataLeaf, bindings genDataBindings) error {
	if leaf != genDataContainers && leaf != genDataNetwork && leaf != genDataAll {
		return errors.New("unknown gen-data leaf")
	}
	if bindings.NewClient == nil {
		return errors.New("gen-data client binding is unavailable")
	}
	client, err := bindings.NewClient(bindings.Prompt, bindings.Warn)
	if err != nil {
		return err
	}
	if client == nil {
		return errors.New("gen-data client binding returned no provider")
	}
	if err := client.Authenticate(ctx); err != nil {
		return err
	}
	var inputs []gendata.MachineInput
	if leaf != genDataNetwork {
		if bindings.Machines == nil {
			return errors.New("gen-data machine binding is unavailable")
		}
		machines, err := bindings.Machines(ctx)
		if err != nil {
			return err
		}
		inputs = make([]gendata.MachineInput, len(machines))
		for i, m := range machines {
			inputs[i] = gendata.MachineInputFromAttributes(m.Name, m.Attributes)
		}
	}
	literal := bindings.Literal
	if literal == nil {
		literal = genDataNixLiteral
	}
	generator := gendata.Generator{Root: root, Source: client, Literal: literal}
	switch leaf {
	case genDataContainers:
		return generator.Containers(ctx, inputs)
	case genDataNetwork:
		return generator.Network(ctx)
	default:
		return generator.All(ctx, inputs)
	}
}
