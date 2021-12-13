package client

type Caesium interface {
}

func Client() Caesium {
	return &client{}
}

type client struct {
}
