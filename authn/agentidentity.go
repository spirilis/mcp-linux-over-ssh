// Adapted from mcp-kube-baker internal/authn (local issuer only).

package authn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
)

// agentInfoAudience must be among the RemoteMCPServer's actorJwt.audiences for the lookup to work.
const agentInfoAudience = "kagent-agent-info"

const resolveMethod = "/kagent.api.v1.AgentIdentityService/ResolveAgentIdentity"

// AgentIdentity is the Agent kagent resolves behind an actor (session) JWT.
type AgentIdentity struct {
	Namespace string
	Name      string
	UID       string
	Labels    map[string]string
	SessionID string
}

// AgentResolver maps a verified actor JWT to its kagent Agent.
type AgentResolver interface {
	Resolve(ctx context.Context, rawJWT string) (AgentIdentity, error)
}

// KagentResolverConfig configures the gRPC lookup against the kagent controller.
type KagentResolverConfig struct {
	// Address is host:port of the controller's gRPC listener, e.g. kagent-controller.kagent:8083.
	Address string `json:"address"`
	// TLS enables TLS; CAFile optionally pins the trusted roots. kagent's controller serves
	// plaintext gRPC inside the cluster by default, which is why TLS is opt-in here: the
	// lookup never leaves the local cluster.
	TLS    bool   `json:"tls"`
	CAFile string `json:"caFile,omitempty"`
}

// KagentResolver calls kagent.api.v1.AgentIdentityService/ResolveAgentIdentity. The messages are
// tiny, so they are encoded by hand with protowire instead of importing kagent's Go module.
type KagentResolver struct {
	conn *grpc.ClientConn
}

// NewKagentResolver dials lazily (grpc.NewClient does not connect until first use).
func NewKagentResolver(cfg KagentResolverConfig) (*KagentResolver, error) {
	if cfg.Address == "" {
		return nil, errors.New("kagent controller address is required")
	}
	creds := insecure.NewCredentials()
	if cfg.TLS {
		tlsCfg, err := tlsConfigFromCA(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(tlsCfg)
	}
	conn, err := grpc.NewClient(cfg.Address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("kagent controller client: %w", err)
	}
	return &KagentResolver{conn: conn}, nil
}

// Close closes the gRPC connection.
func (k *KagentResolver) Close() error { return k.conn.Close() }

// Resolve forwards the caller's own actor JWT; kagent checks it carries kagent-agent-info.
func (k *KagentResolver) Resolve(ctx context.Context, rawJWT string) (AgentIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+rawJWT)
	var out rawMessage
	if err := k.conn.Invoke(ctx, resolveMethod, rawMessage{}, &out, grpc.ForceCodec(rawCodec{})); err != nil {
		return AgentIdentity{}, fmt.Errorf("ResolveAgentIdentity: %w", err)
	}
	id, err := decodeResolveResponse(out)
	if err != nil {
		return AgentIdentity{}, err
	}
	if id.Namespace == "" || id.Name == "" {
		return AgentIdentity{}, errors.New("ResolveAgentIdentity returned no agent")
	}
	return id, nil
}

// rawMessage is an already-encoded protobuf message.
type rawMessage []byte

type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }

func (rawCodec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case rawMessage:
		return m, nil
	case *rawMessage:
		return *m, nil
	}
	return nil, fmt.Errorf("rawCodec cannot marshal %T", v)
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	m, ok := v.(*rawMessage)
	if !ok {
		return fmt.Errorf("rawCodec cannot unmarshal into %T", v)
	}
	*m = append((*m)[:0], data...)
	return nil
}

// decodeResolveResponse decodes ResolveAgentIdentityResponse:
//
//	message ResolveAgentIdentityResponse { AgentIdentity agent = 1; string session_id = 2; string runtime_revision = 3; }
//	message AgentIdentity { string namespace = 1; string name = 2; string uid = 3; map<string,string> labels = 4; map<string,string> annotations = 5; }
func decodeResolveResponse(b []byte) (AgentIdentity, error) {
	var id AgentIdentity
	err := walkFields(b, func(num protowire.Number, typ protowire.Type, v []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			return walkFields(v, func(n protowire.Number, t protowire.Type, f []byte) error {
				if t != protowire.BytesType {
					return nil
				}
				switch n {
				case 1:
					id.Namespace = string(f)
				case 2:
					id.Name = string(f)
				case 3:
					id.UID = string(f)
				case 4:
					k, val, err := decodeMapEntry(f)
					if err != nil {
						return err
					}
					if id.Labels == nil {
						id.Labels = map[string]string{}
					}
					id.Labels[k] = val
				}
				return nil
			})
		case num == 2 && typ == protowire.BytesType:
			id.SessionID = string(v)
		}
		return nil
	})
	return id, err
}

func decodeMapEntry(b []byte) (key, val string, err error) {
	err = walkFields(b, func(n protowire.Number, t protowire.Type, f []byte) error {
		if t == protowire.BytesType {
			switch n {
			case 1:
				key = string(f)
			case 2:
				val = string(f)
			}
		}
		return nil
	})
	return key, val, err
}

// walkFields calls fn for every field; v holds the payload for bytes fields and nil otherwise.
func walkFields(b []byte, fn func(protowire.Number, protowire.Type, []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("decode protobuf tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		var payload []byte
		if typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return fmt.Errorf("decode protobuf bytes: %w", protowire.ParseError(m))
			}
			payload, n = v, m
		} else {
			n = protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return fmt.Errorf("decode protobuf field: %w", protowire.ParseError(n))
			}
		}
		if err := fn(num, typ, payload); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
