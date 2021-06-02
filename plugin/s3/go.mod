module github.com/micro/micro/plugin/s3/v3

go 1.15

require (
	github.com/aws/aws-sdk-go v1.23.0
	github.com/micro/micro/v3 v3.2.2-0.20210520154937-d69eb589fa2a
	github.com/stretchr/testify v1.7.0
)

replace github.com/micro/micro/v3 => ../..
