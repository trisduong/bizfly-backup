package s3

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	storage "github.com/aws/aws-sdk-go/service/s3"
	"github.com/cenkalti/backoff"
	"github.com/spf13/viper"

	"github.com/bizflycloud/bizfly-backup/pkg/backupapi"
	"github.com/bizflycloud/bizfly-backup/pkg/limiter"
	"github.com/bizflycloud/bizfly-backup/pkg/storage_vault"
)

type S3 struct {
	Id               string
	ActionID         string
	Name             string
	StorageBucket    string
	SecretRef        string
	CredentialType   string
	StorageVaultType string
	Location         string
	Region           string
	S3Session        *storage.S3

	logger       *zap.Logger
	backupClient *backupapi.Client
}

func (s3 *S3) Type() storage_vault.Type {
	tpe := storage_vault.Type{
		StorageVaultType: s3.StorageVaultType,
		CredentialType:   s3.CredentialType,
	}
	return tpe
}

func (s3 *S3) ID() (string, string) {
	return s3.Id, s3.ActionID
}

var _ storage_vault.StorageVault = (*S3)(nil)
var uploadKb, downloadKb int

func NewS3Default(vault backupapi.StorageVault, actionID string, limitUpload, limitDownload int, backupClient *backupapi.Client) (*S3, error) {
	uploadKb, downloadKb = limitUpload, limitDownload

	s3 := &S3{
		Id:               vault.ID,
		ActionID:         actionID,
		Name:             vault.Name,
		StorageBucket:    vault.StorageBucket,
		SecretRef:        vault.SecretRef,
		CredentialType:   vault.CredentialType,
		StorageVaultType: vault.StorageVaultType,
		Location:         vault.Credential.AwsLocation,
		Region:           vault.Credential.Region,
		backupClient:     backupClient,
	}

	if s3.logger == nil {
		l, err := backupapi.WriteLog()
		if err != nil {
			return nil, err
		}
		s3.logger = l
	}

	cred := credentials.NewStaticCredentials(vault.Credential.AwsAccessKeyId, vault.Credential.AwsSecretAccessKey, vault.Credential.Token)
	_, err := cred.Get()
	if err != nil {
		s3.logger.Error("Bad credentials", zap.Error(err))
	}

	// using a Custom HTTP Transport
	rt, err := storage_vault.Transport(storage_vault.TransportOptions{
		Connect:          30 * time.Second,
		ExpectContinue:   1 * time.Second,
		IdleConn:         90 * time.Second,
		ConnKeepAlive:    30 * time.Second,
		MaxAllIdleConns:  100,
		MaxHostIdleConns: 100,
		ResponseHeader:   10 * time.Second,
		TLSHandshake:     10 * time.Second,
	})
	if err != nil {
		s3.logger.Error("Got an error creating custom HTTP client", zap.Error(err))
	}

	// wrap the transport so that the throughput via HTTP is limited
	lim := limiter.NewStaticLimiter(limitUpload, limitDownload)
	rt = lim.Transport(rt)

	sess := storage.New(session.Must(session.NewSession(&aws.Config{
		DisableSSL:       aws.Bool(false),
		Credentials:      cred,
		Endpoint:         aws.String(vault.Credential.AwsLocation),
		Region:           aws.String(vault.Credential.Region),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient:       &http.Client{Transport: rt},
	})))
	s3.S3Session = sess
	return s3, nil

}

type HTTPClient struct{}

var (
	HttpClient = HTTPClient{}
)

const (
	maxRetry = 3 * time.Minute
)

func (s3 *S3) VerifyObject(key string) (bool, bool, string, error) {
	var isExist bool
	var integrity bool
	var etag string
	var err error
	bo := backoff.NewExponentialBackOff()
	bo.MaxInterval = maxRetry
	bo.MaxElapsedTime = maxRetry

	for {
		isExist, etag, err = s3.HeadObject(key)
		if err == nil {
			if isExist {
				integrity = strings.Contains(etag, key)
			}
			break
		}
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "NotFound" {
				err = nil
				break
			}
			s3.logger.Sugar().Errorf("VerifyObject error: %s %s", aerr.Code(), aerr.Message())
			if (aerr.Code() == "AccessDenied" || aerr.Code() == "Forbidden") && s3.Type().CredentialType == "DEFAULT" {
				s3.logger.Sugar().Info("GetCredential in head object ", key)
				storageVaultID, actID := s3.ID()
				vault, err := s3.backupClient.GetCredentialStorageVault(storageVaultID, actID, nil)
				if err != nil {
					s3.logger.Error("Error get credential", zap.Error(err))
					break
				}

				err = s3.RefreshCredential(vault.Credential)
				if err != nil {
					s3.logger.Error("Error refresh credential ", zap.Error(err))
					break
				}
			}
		}

		s3.logger.Error("VerifyObject. Retrying", zap.Error(err))
		d := bo.NextBackOff()
		if d == backoff.Stop {
			s3.logger.Debug("VerifyObject. Retry time out")
			break
		}
		s3.logger.Sugar().Info("VerifyObject. Retry in ", d)
	}
	return isExist, integrity, etag, err
}

func (s3 *S3) PutObject(key string, data []byte) error {
	var err error
	var once bool
	bo := backoff.NewExponentialBackOff()
	bo.MaxInterval = maxRetry
	bo.MaxElapsedTime = maxRetry
	for {
		isExist, integrity, _, _ := s3.VerifyObject(key)
		if isExist {
			if !integrity {
				_, err = s3.S3Session.PutObject(&storage.PutObjectInput{
					Bucket: aws.String(s3.StorageBucket),
					Key:    aws.String(key),
					Body:   bytes.NewReader(data),
				})
				if err == nil {
					break
				}
			} else {
				break
			}
		} else {
			_, err = s3.S3Session.PutObject(&storage.PutObjectInput{
				Bucket: aws.String(s3.StorageBucket),
				Key:    aws.String(key),
				Body:   bytes.NewReader(data),
			})
			if !strings.Contains(key, "chunk.json") && !strings.Contains(key, "index.json") && !strings.Contains(key, "file.csv") {
				isExist, integrity, _, _ = s3.VerifyObject(key)
				if isExist {
					if !integrity {
						_, err = s3.S3Session.PutObject(&storage.PutObjectInput{
							Bucket: aws.String(s3.StorageBucket),
							Key:    aws.String(key),
							Body:   bytes.NewReader(data),
						})
						if err == nil {
							break
						}
					} else {
						break
					}
				}
			}
			if err == nil {
				break
			}
		}
		if aerr, ok := err.(awserr.Error); ok {
			s3.logger.Sugar().Errorf("PutObject error: %s %s", aerr.Code(), aerr.Message())
			if aerr.Code() == "AccessDenied" || aerr.Code() == "Forbidden" {
				if once {
					s3.logger.Error("Return false cause in put object: ", zap.Error(err), zap.String("code", aerr.Code()), zap.String("key", key))
					return err
				}
				s3.logger.Info("Put object one more time")
				once = true
				rand.Seed(time.Now().UnixNano())
				n := rand.Intn(3) // n will be between 0 and 10
				time.Sleep(time.Duration(n) * time.Second)
			}
		}
		s3.logger.Debug("PutObject error. Retrying")
		d := bo.NextBackOff()
		if d == backoff.Stop {
			s3.logger.Debug("PutObject error. Retry time out")
			break
		}
		s3.logger.Sugar().Info("PutObject error. Retry in ", d)
		time.Sleep(d)
	}

	return err
}

func (s3 *S3) GetObject(key string) ([]byte, error) {
	var err error
	var once bool
	bo := backoff.NewExponentialBackOff()
	bo.MaxInterval = maxRetry
	bo.MaxElapsedTime = maxRetry
	var obj *storage.GetObjectOutput
	for {
		obj, err = s3.S3Session.GetObject(&storage.GetObjectInput{
			Bucket: aws.String(s3.StorageBucket),
			Key:    aws.String(key),
		})
		if err == nil {
			break
		}

		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "NoSuchKey" {
				return nil, err
			}

			s3.logger.Sugar().Errorf("GetObject error: %s %s", aerr.Code(), aerr.Message())
			if aerr.Code() == "AccessDenied" || aerr.Code() == "Forbidden" {
				if once {
					s3.logger.Error("Return false cause in get object: ", zap.Error(err), zap.String("code", aerr.Code()), zap.String("key", key))
					return nil, err
				}
				s3.logger.Sugar().Info("Get object one more time ", key)
				once = true
				rand.Seed(time.Now().UnixNano())
				n := rand.Intn(3) // n will be between 0 and 10
				time.Sleep(time.Duration(n) * time.Second)
			} else {
				return nil, err
			}
		}
		s3.logger.Debug("GetObject error. Retrying")
		d := bo.NextBackOff()
		if d == backoff.Stop {
			s3.logger.Debug("GetObject error. Retry time out")
			break
		}
		s3.logger.Sugar().Info("GetObject error. Retry in ", d)
		time.Sleep(d)
	}

	body, err := ioutil.ReadAll(obj.Body)

	return body, err
}

func (s3 *S3) HeadObject(key string) (bool, string, error) {
	var err error
	var headObject *storage.HeadObjectOutput
	var once bool
	bo := backoff.NewExponentialBackOff()
	bo.MaxInterval = maxRetry
	bo.MaxElapsedTime = maxRetry
	for {
		headObject, err = s3.S3Session.HeadObject(&storage.HeadObjectInput{
			Bucket: aws.String(s3.StorageBucket),
			Key:    aws.String(key),
		})
		if err == nil {
			return true, *headObject.ETag, nil
		}

		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "NotFound" {
				return false, "", err
			}

			s3.logger.Sugar().Errorf("HeadObject error: %s %s", aerr.Code(), aerr.Message())
			if aerr.Code() == "AccessDenied" || aerr.Code() == "Forbidden" {
				if once {
					s3.logger.Error("Return false cause in head object: ", zap.Error(err), zap.String("code", aerr.Code()), zap.String("key", key))
					return false, "", err
				}
				s3.logger.Sugar().Info("Head object one more time ", key)
				once = true
				rand.Seed(time.Now().UnixNano())
				n := rand.Intn(3) // n will be between 0 and 10
				time.Sleep(time.Duration(n) * time.Second)
			}
		}
		s3.logger.Debug("Head object error. Retrying")
		d := bo.NextBackOff()
		if d == backoff.Stop {
			s3.logger.Debug("Head object error. Retry time out", zap.Error(err))
			break
		}
		s3.logger.Sugar().Info("Head object error. Retry in ", d)
		time.Sleep(d)

	}
	return false, "", err
}

func (s3 *S3) RefreshCredential(credential storage_vault.Credential) error {
	cred := credentials.NewStaticCredentials(credential.AwsAccessKeyId, credential.AwsSecretAccessKey, credential.Token)
	_, err := cred.Get()
	if err != nil {
		s3.logger.Error("err ", zap.Error(err))
		return err
	}

	// using a Custom HTTP Transport
	rt, err := storage_vault.Transport(storage_vault.TransportOptions{
		Connect:          30 * time.Second,
		ExpectContinue:   1 * time.Second,
		IdleConn:         90 * time.Second,
		ConnKeepAlive:    30 * time.Second,
		MaxAllIdleConns:  100,
		MaxHostIdleConns: 100,
		ResponseHeader:   10 * time.Second,
		TLSHandshake:     10 * time.Second,
	})
	if err != nil {
		s3.logger.Error("Got an error creating custom HTTP client", zap.Error(err))
	}

	if uploadKb == 0 {
		uploadKb = viper.GetInt("limit_upload")
	}
	if downloadKb == 0 {
		downloadKb = viper.GetInt("limit_download")
	}

	// wrap the transport so that the throughput via HTTP is limited
	lim := limiter.NewStaticLimiter(uploadKb, downloadKb)
	rt = lim.Transport(rt)

	sess := storage.New(session.Must(session.NewSession(&aws.Config{
		DisableSSL:       aws.Bool(false),
		Credentials:      cred,
		Endpoint:         aws.String(s3.Location),
		Region:           aws.String(s3.Region),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient:       &http.Client{Transport: rt},
	})))
	s3.S3Session = sess
	s3.logger.Info("Refresh credential success")
	return nil
}
