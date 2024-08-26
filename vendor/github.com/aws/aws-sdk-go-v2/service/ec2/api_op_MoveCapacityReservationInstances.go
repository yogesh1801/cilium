// Code generated by smithy-go-codegen DO NOT EDIT.

package ec2

import (
	"context"
	"fmt"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Move available capacity from a source Capacity Reservation to a destination
// Capacity Reservation. The source Capacity Reservation and the destination
// Capacity Reservation must be active , owned by your Amazon Web Services account,
// and share the following:
//
//   - Instance type
//
//   - Platform
//
//   - Availability Zone
//
//   - Tenancy
//
//   - Placement group
//
//   - Capacity Reservation end time - At specific time or Manually .
func (c *Client) MoveCapacityReservationInstances(ctx context.Context, params *MoveCapacityReservationInstancesInput, optFns ...func(*Options)) (*MoveCapacityReservationInstancesOutput, error) {
	if params == nil {
		params = &MoveCapacityReservationInstancesInput{}
	}

	result, metadata, err := c.invokeOperation(ctx, "MoveCapacityReservationInstances", params, optFns, c.addOperationMoveCapacityReservationInstancesMiddlewares)
	if err != nil {
		return nil, err
	}

	out := result.(*MoveCapacityReservationInstancesOutput)
	out.ResultMetadata = metadata
	return out, nil
}

type MoveCapacityReservationInstancesInput struct {

	//  The ID of the Capacity Reservation that you want to move capacity into.
	//
	// This member is required.
	DestinationCapacityReservationId *string

	// The number of instances that you want to move from the source Capacity
	// Reservation.
	//
	// This member is required.
	InstanceCount *int32

	//  The ID of the Capacity Reservation from which you want to move capacity.
	//
	// This member is required.
	SourceCapacityReservationId *string

	// Unique, case-sensitive identifier that you provide to ensure the idempotency of
	// the request. For more information, see [Ensure Idempotency].
	//
	// [Ensure Idempotency]: https://docs.aws.amazon.com/AWSEC2/latest/APIReference/Run_Instance_Idempotency.html
	ClientToken *string

	// Checks whether you have the required permissions for the action, without
	// actually making the request, and provides an error response. If you have the
	// required permissions, the error response is DryRunOperation . Otherwise, it is
	// UnauthorizedOperation .
	DryRun *bool

	noSmithyDocumentSerde
}

type MoveCapacityReservationInstancesOutput struct {

	//  Information about the destination Capacity Reservation.
	DestinationCapacityReservation *types.CapacityReservation

	//  The number of instances that were moved from the source Capacity Reservation
	// to the destination Capacity Reservation.
	InstanceCount *int32

	//  Information about the source Capacity Reservation.
	SourceCapacityReservation *types.CapacityReservation

	// Metadata pertaining to the operation's result.
	ResultMetadata middleware.Metadata

	noSmithyDocumentSerde
}

func (c *Client) addOperationMoveCapacityReservationInstancesMiddlewares(stack *middleware.Stack, options Options) (err error) {
	if err := stack.Serialize.Add(&setOperationInputMiddleware{}, middleware.After); err != nil {
		return err
	}
	err = stack.Serialize.Add(&awsEc2query_serializeOpMoveCapacityReservationInstances{}, middleware.After)
	if err != nil {
		return err
	}
	err = stack.Deserialize.Add(&awsEc2query_deserializeOpMoveCapacityReservationInstances{}, middleware.After)
	if err != nil {
		return err
	}
	if err := addProtocolFinalizerMiddlewares(stack, options, "MoveCapacityReservationInstances"); err != nil {
		return fmt.Errorf("add protocol finalizers: %v", err)
	}

	if err = addlegacyEndpointContextSetter(stack, options); err != nil {
		return err
	}
	if err = addSetLoggerMiddleware(stack, options); err != nil {
		return err
	}
	if err = addClientRequestID(stack); err != nil {
		return err
	}
	if err = addComputeContentLength(stack); err != nil {
		return err
	}
	if err = addResolveEndpointMiddleware(stack, options); err != nil {
		return err
	}
	if err = addComputePayloadSHA256(stack); err != nil {
		return err
	}
	if err = addRetry(stack, options); err != nil {
		return err
	}
	if err = addRawResponseToMetadata(stack); err != nil {
		return err
	}
	if err = addRecordResponseTiming(stack); err != nil {
		return err
	}
	if err = addClientUserAgent(stack, options); err != nil {
		return err
	}
	if err = smithyhttp.AddErrorCloseResponseBodyMiddleware(stack); err != nil {
		return err
	}
	if err = smithyhttp.AddCloseResponseBodyMiddleware(stack); err != nil {
		return err
	}
	if err = addSetLegacyContextSigningOptionsMiddleware(stack); err != nil {
		return err
	}
	if err = addTimeOffsetBuild(stack, c); err != nil {
		return err
	}
	if err = addUserAgentRetryMode(stack, options); err != nil {
		return err
	}
	if err = addIdempotencyToken_opMoveCapacityReservationInstancesMiddleware(stack, options); err != nil {
		return err
	}
	if err = addOpMoveCapacityReservationInstancesValidationMiddleware(stack); err != nil {
		return err
	}
	if err = stack.Initialize.Add(newServiceMetadataMiddleware_opMoveCapacityReservationInstances(options.Region), middleware.Before); err != nil {
		return err
	}
	if err = addRecursionDetection(stack); err != nil {
		return err
	}
	if err = addRequestIDRetrieverMiddleware(stack); err != nil {
		return err
	}
	if err = addResponseErrorMiddleware(stack); err != nil {
		return err
	}
	if err = addRequestResponseLogging(stack, options); err != nil {
		return err
	}
	if err = addDisableHTTPSMiddleware(stack, options); err != nil {
		return err
	}
	return nil
}

type idempotencyToken_initializeOpMoveCapacityReservationInstances struct {
	tokenProvider IdempotencyTokenProvider
}

func (*idempotencyToken_initializeOpMoveCapacityReservationInstances) ID() string {
	return "OperationIdempotencyTokenAutoFill"
}

func (m *idempotencyToken_initializeOpMoveCapacityReservationInstances) HandleInitialize(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
	out middleware.InitializeOutput, metadata middleware.Metadata, err error,
) {
	if m.tokenProvider == nil {
		return next.HandleInitialize(ctx, in)
	}

	input, ok := in.Parameters.(*MoveCapacityReservationInstancesInput)
	if !ok {
		return out, metadata, fmt.Errorf("expected middleware input to be of type *MoveCapacityReservationInstancesInput ")
	}

	if input.ClientToken == nil {
		t, err := m.tokenProvider.GetIdempotencyToken()
		if err != nil {
			return out, metadata, err
		}
		input.ClientToken = &t
	}
	return next.HandleInitialize(ctx, in)
}
func addIdempotencyToken_opMoveCapacityReservationInstancesMiddleware(stack *middleware.Stack, cfg Options) error {
	return stack.Initialize.Add(&idempotencyToken_initializeOpMoveCapacityReservationInstances{tokenProvider: cfg.IdempotencyTokenProvider}, middleware.Before)
}

func newServiceMetadataMiddleware_opMoveCapacityReservationInstances(region string) *awsmiddleware.RegisterServiceMetadata {
	return &awsmiddleware.RegisterServiceMetadata{
		Region:        region,
		ServiceID:     ServiceID,
		OperationName: "MoveCapacityReservationInstances",
	}
}
